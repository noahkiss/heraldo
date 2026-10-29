// Package llm is heraldo's client for an OpenAI-compatible LLM endpoint,
// and the chat role that answers questions about stored events.
//
// The LLM is optional and off by default (llm.mode off). It runs on the
// primary only: on a standby every call fails with ErrStandby. Everything
// the server does works without it, and every caller falls back to its
// deterministic path when a call fails.
//
// Every call goes through Client.Call, the one place that applies the
// guards:
//   - the mode (off refuses every call but the operator's test),
//   - the circuit breaker (3 failures in a row open it for 10 minutes),
//   - the budgets (llm.tokens_per_day and llm.calls_per_minute; past one,
//     the effective mode is at most shadow until midnight),
//   - redaction (llm.redact) of every message before it leaves,
//   - the cache (sha256 of role, model, prompt version, guidance versions
//     and the normalized input; 24 hours),
//   - the call log (llm_calls), which keeps the output, never the prompt.
//
// One passive heraldo notice is stored per outage: when the circuit opens
// or a budget is passed, and not again until a call succeeds.
//
// The token (llm.token) is never logged, stored in the call log or
// returned.
package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/ids"
	"github.com/noahkiss/heraldo/internal/settings"
	"github.com/noahkiss/heraldo/internal/store"
)

// Roles name what a call is for, in the call log.
const (
	RoleChat      = "chat"
	RoleDecisions = "decisions"
	RoleTest      = "test"
)

// Modes are the values of llm.mode.
const (
	ModeOff     = "off"
	ModeShadow  = "shadow"
	ModeAdvise  = "advise"
	ModeEnforce = "enforce"
)

// Guard limits.
const (
	CircuitFailures = 3
	CircuitOpenFor  = 10 * time.Minute
	CacheFor        = 24 * time.Hour
	// KeepCalls is how long the call log keeps a row.
	KeepCalls = 30 * 24 * time.Hour
)

// NoticeSource and NoticeThread are the outage notice's source and thread.
const (
	NoticeSource = "heraldo"
	NoticeThread = "llm"
)

// Errors a Call returns before any request leaves the server.
var (
	ErrOff           = errors.New("the LLM is off (llm.mode off)")
	ErrStandby       = errors.New("the LLM never runs on a standby")
	ErrNotConfigured = errors.New("the LLM is not configured: set llm.base_url and a model")
	ErrCircuitOpen   = errors.New("the LLM circuit is open after repeated failures")
	ErrBudget        = errors.New("the LLM budget is used up")
	// ErrInvalid wraps an output that failed the caller's validation.
	ErrInvalid = errors.New("the LLM's output failed validation")
)

// CallLog is the llm_calls table. *store.Store is one.
type CallLog interface {
	AddLLMCall(ctx context.Context, c store.LLMCall) error
	CachedLLMCall(ctx context.Context, hash string, since time.Time) (store.LLMCall, error)
	LLMTokensSince(ctx context.Context, since time.Time) (int64, error)
}

// Config configures a Client.
type Config struct {
	// Settings holds the llm.* keys. nil means settings.Default().
	Settings *settings.Manager
	// Log is the call log; nil logs nothing and caches nothing, and the
	// daily token budget counts only this process's calls.
	Log CallLog
	// Notify stores a heraldo event from fields. The client calls it once
	// per outage with a passive message. nil stores nothing.
	Notify func(ctx context.Context, fields map[string]any) error
	// Standby is set on a standby server: every call fails.
	Standby bool
	// NewProvider builds the provider for llm.api, base URL and token. nil
	// means the OpenAI-compatible ones. Tests pass one that returns a Fake.
	NewProvider func(api, baseURL, token string) Provider
	// HTTP is the client the real providers use; nil means one with no
	// timeout of its own (each call has the role's timeout).
	HTTP   *http.Client
	Now    func() time.Time
	Logger *slog.Logger
}

// Client is the LLM provider with its guards. It is safe for concurrent
// use.
type Client struct {
	cfg Config

	mu          sync.Mutex
	failures    int
	openUntil   time.Time
	shadowUntil time.Time
	calls       []time.Time // real calls in the last minute
	tokens      int64       // tokens since tokensDay, when Log is nil
	tokensDay   time.Time
	noticed     bool
	lastErr     string
	lastErrAt   time.Time
	lastPrune   time.Time
}

// New returns a Client.
func New(cfg Config) *Client {
	if cfg.Settings == nil {
		cfg.Settings = settings.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{}
	}
	if cfg.NewProvider == nil {
		hc := cfg.HTTP
		cfg.NewProvider = func(apiKind, baseURL, token string) Provider {
			if apiKind == "openai-responses" {
				return &OpenAIResponses{BaseURL: baseURL, Token: token, HTTP: hc}
			}
			return &OpenAIChat{BaseURL: baseURL, Token: token, HTTP: hc}
		}
	}
	return &Client{cfg: cfg}
}

func (c *Client) str(name string) string { return settings.From[string](c.cfg.Settings, name) }

// Mode is llm.mode as set.
func (c *Client) Mode() string { return c.str("llm.mode") }

// Policy is what of an event may leave the server, from the settings.
func (c *Client) Policy() Policy {
	m := c.cfg.Settings
	return NewPolicy(settings.From[int](m, "llm.send_body_chars"), settings.From[[]string](m, "llm.redact"),
		settings.From[[]string](m, "llm.exclude_projects"))
}

// Model is the model for role: llm.chat_model for chat, else
// llm.decisions_model. Either falls back to the other when empty.
func (c *Client) Model(role string) string {
	chat, dec := c.str("llm.chat_model"), c.str("llm.decisions_model")
	if role == RoleDecisions {
		if dec != "" {
			return dec
		}
		return chat
	}
	if chat != "" {
		return chat
	}
	return dec
}

func (c *Client) timeout(role string) time.Duration {
	if role == RoleChat {
		return settings.From[time.Duration](c.cfg.Settings, "llm.chat_timeout")
	}
	return settings.From[time.Duration](c.cfg.Settings, "llm.decisions_timeout")
}

// Timeout is how long one call for role may take.
func (c *Client) Timeout(role string) time.Duration { return c.timeout(role) }

// BaseHost is the endpoint's host, for logs and status. Never a path.
func (c *Client) BaseHost() string {
	u, err := url.Parse(c.str("llm.base_url"))
	if err != nil {
		return ""
	}
	return u.Host
}

// Configured reports whether a base URL and a model are set.
func (c *Client) Configured() bool {
	return c.str("llm.base_url") != "" && c.Model(RoleChat) != ""
}

// nextMidnight is the first midnight after t, in t's zone.
func nextMidnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, t.Location())
}

func midnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// EffectiveMode is the mode in force now: off on a standby; at most shadow
// while the circuit is open and until midnight after a budget was passed.
func (c *Client) EffectiveMode() string {
	mode := c.Mode()
	if c.cfg.Standby || mode == ModeOff {
		return ModeOff
	}
	now := c.cfg.Now()
	c.mu.Lock()
	degraded := now.Before(c.shadowUntil) || now.Before(c.openUntil)
	c.mu.Unlock()
	if degraded && (mode == ModeAdvise || mode == ModeEnforce) {
		return ModeShadow
	}
	return mode
}

// ChatAllowed reports whether a question may go to the chat model: llm.chat
// on, llm.mode not off, and not a standby.
func (c *Client) ChatAllowed() bool {
	return !c.cfg.Standby && c.Mode() != ModeOff && settings.From[bool](c.cfg.Settings, "llm.chat")
}

// Health is what /healthz reports as llm.
func (c *Client) Health() api.HealthLlm {
	if c.cfg.Standby || c.Mode() == ModeOff {
		return api.HealthLlmOff
	}
	now := c.cfg.Now()
	c.mu.Lock()
	degraded := now.Before(c.shadowUntil) || now.Before(c.openUntil)
	c.mu.Unlock()
	if degraded || !c.Configured() {
		return api.HealthLlmDegraded
	}
	return api.HealthLlmOn
}

// Status is GET /v1/llm/status.
func (c *Client) Status(ctx context.Context) api.LlmStatus {
	now := c.cfg.Now()
	m := c.cfg.Settings
	tokens, _ := c.tokensToday(ctx, now)
	opt := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	tokenSet := false
	if v, err := m.View("llm.token"); err == nil {
		tokenSet = v.Set
	}
	out := api.LlmStatus{
		Mode: api.LlmStatusMode(c.Mode()), EffectiveMode: api.LlmStatusEffectiveMode(c.EffectiveMode()),
		Chat: c.ChatAllowed(), Configured: c.Configured(), TokenSet: tokenSet,
		Api: opt(c.str("llm.api")), BaseHost: opt(c.BaseHost()),
		ChatModel: opt(c.str("llm.chat_model")), DecisionsModel: opt(c.str("llm.decisions_model")),
		TokensToday: tokens, TokensPerDay: settings.From[int](m, "llm.tokens_per_day"),
		CallsPerMinute: settings.From[int](m, "llm.calls_per_minute"), Circuit: api.LlmStatusCircuitClosed,
	}
	if c.cfg.Standby {
		out.Standby = ptr(true)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out.CallsLastMinute = len(c.recentCalls(now))
	out.Failures = ptr(c.failures)
	if now.Before(c.openUntil) {
		out.Circuit, out.CircuitUntil = api.LlmStatusCircuitOpen, ptr(c.openUntil.UTC())
	}
	if now.Before(c.shadowUntil) {
		out.ShadowUntil = ptr(c.shadowUntil.UTC())
	}
	if c.lastErr != "" {
		out.LastError, out.LastErrorAt = ptr(c.lastErr), ptr(c.lastErrAt.UTC())
	}
	return out
}

func ptr[T any](v T) *T { return &v }

// recentCalls drops calls older than a minute and returns the rest. The
// caller holds mu.
func (c *Client) recentCalls(now time.Time) []time.Time {
	cut := now.Add(-time.Minute)
	i := 0
	for i < len(c.calls) && !c.calls[i].After(cut) {
		i++
	}
	c.calls = c.calls[i:]
	return c.calls
}

func (c *Client) tokensToday(ctx context.Context, now time.Time) (int64, error) {
	day := midnight(now)
	if c.cfg.Log != nil {
		return c.cfg.Log.LLMTokensSince(ctx, day)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.tokensDay.Equal(day) {
		c.tokens, c.tokensDay = 0, day
	}
	return c.tokens, nil
}

// Call is one request through the guards.
type Call struct {
	Role string
	// Prompt is the prompt file the request is built from; its version is
	// logged and hashed.
	Prompt Prompt
	// GuidanceVersions are the guidance lines the request carries, as
	// id:version, for the cache key.
	GuidanceVersions []string
	// Input is the normalized input, for the cache key. Callers normalize
	// what should not break a cache hit, such as times and counters.
	Input any
	// Cache serves a status ok answer to the same key from the last 24
	// hours instead of calling.
	Cache bool
	// Request is sent with redacted messages. An empty Model is the role's
	// model; an empty Structured is llm.structured.
	Request Request
	// Validate checks the answer. An error marks the call invalid, counts
	// as a failure, and Call returns it wrapped in ErrInvalid.
	Validate func(Response) error
	// Force runs with llm.mode off: the operator's test call.
	Force bool
}

// Result is a call's answer.
type Result struct {
	Response
	// CallID is the llm_calls row; empty without a call log.
	CallID  string
	Cached  bool
	Model   string
	Latency time.Duration
}

// Key is the cache key of a call: sha256 of role, model, prompt version,
// guidance versions and input.
func Key(role, model, promptVersion string, guidanceVersions []string, input any) string {
	b, _ := json.Marshal(input)
	h := sha256.New()
	for _, part := range []string{role, model, promptVersion, strings.Join(guidanceVersions, ","), string(b)} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Call sends one request through the guards. A guard that refuses returns
// one of the Err values and sends nothing.
func (c *Client) Call(ctx context.Context, call Call) (Result, error) {
	if c.cfg.Standby {
		return Result{}, ErrStandby
	}
	if c.Mode() == ModeOff && !call.Force {
		return Result{}, ErrOff
	}
	base, model := c.str("llm.base_url"), call.Request.Model
	if model == "" {
		model = c.Model(call.Role)
	}
	if base == "" || model == "" {
		return Result{}, ErrNotConfigured
	}
	req := call.Request
	req.Model = model
	if req.Structured == "" {
		req.Structured = c.str("llm.structured")
	}
	pol := c.Policy()
	req.Messages = make([]Message, len(call.Request.Messages))
	for i, m := range call.Request.Messages {
		m.Content = pol.RedactText(m.Content)
		req.Messages[i] = m
	}
	now := c.cfg.Now()
	hash := Key(call.Role, model, call.Prompt.Version, call.GuidanceVersions, call.Input)
	row := store.LLMCall{Role: call.Role, Model: model, BaseHost: c.BaseHost(), At: now.UTC(), InputHash: hash,
		PromptVersion: call.Prompt.Version}

	if call.Cache && c.cfg.Log != nil {
		if hit, err := c.cfg.Log.CachedLLMCall(ctx, hash, now.Add(-CacheFor)); err == nil {
			resp := responseFromOutput(hit.Output)
			if call.Validate == nil || call.Validate(resp) == nil {
				row.Status, row.Output = store.LLMCached, hit.Output
				id := c.log(ctx, row)
				return Result{Response: resp, CallID: id, Cached: true, Model: model}, nil
			}
		}
	}
	if err := c.admit(ctx, now); err != nil {
		return Result{}, err
	}

	token := c.str("llm.token")
	p := c.cfg.NewProvider(c.str("llm.api"), base, token)
	cctx, cancel := context.WithTimeout(ctx, c.timeout(call.Role))
	start := time.Now()
	resp, err := p.Complete(cctx, req)
	cancel()
	lat := time.Since(start)
	row.LatencyMS, row.TokensIn, row.TokensOut = lat.Milliseconds(), resp.TokensIn, resp.TokensOut
	c.addTokens(now, resp.TokensIn+resp.TokensOut)
	if err == nil && call.Validate != nil {
		if verr := call.Validate(resp); verr != nil {
			err = fmt.Errorf("%w: %v", ErrInvalid, verr)
			row.Status = store.LLMInvalid
		}
	}
	if err != nil {
		if row.Status == "" {
			row.Status = store.LLMError
			if errors.Is(err, context.DeadlineExceeded) {
				row.Status = store.LLMTimeout
				err = fmt.Errorf("the endpoint did not answer within %s", c.timeout(call.Role))
			}
		}
		msg := c.clean(err.Error(), token, pol)
		row.Error, row.Output = msg, outputOf(resp)
		id := c.log(ctx, row)
		c.fail(ctx, msg)
		c.cfg.Logger.Warn("llm: call failed", "role", call.Role, "model", model, "host", row.BaseHost,
			"status", row.Status, "err", msg, "call", id)
		if errors.Is(err, ErrInvalid) {
			return Result{CallID: id, Model: model, Latency: lat}, err
		}
		return Result{CallID: id, Model: model, Latency: lat}, errors.New(msg)
	}
	row.Status, row.Output = store.LLMOK, outputOf(resp)
	id := c.log(ctx, row)
	c.succeed()
	return Result{Response: resp, CallID: id, Model: model, Latency: lat}, nil
}

// clean keeps a provider error safe to log and return: never the token,
// and redacted.
func (c *Client) clean(msg, token string, pol Policy) string {
	if token != "" {
		msg = strings.ReplaceAll(msg, token, Redacted)
	}
	msg = pol.RedactText(msg)
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return msg
}

// admit checks the circuit and the budgets, and counts the call.
func (c *Client) admit(ctx context.Context, now time.Time) error {
	perDay := settings.From[int](c.cfg.Settings, "llm.tokens_per_day")
	perMin := settings.From[int](c.cfg.Settings, "llm.calls_per_minute")
	tokens, err := c.tokensToday(ctx, now)
	if err != nil {
		c.cfg.Logger.Error("llm: tokens today", "err", err)
	}
	c.mu.Lock()
	if now.Before(c.openUntil) {
		c.mu.Unlock()
		return ErrCircuitOpen
	}
	var why string
	switch {
	case perDay <= 0 || perMin <= 0:
		why = "a budget is 0"
	case tokens >= int64(perDay):
		why = fmt.Sprintf("the daily token budget (%d) is used up", perDay)
	case len(c.recentCalls(now)) >= perMin:
		why = fmt.Sprintf("more than %d calls in a minute", perMin)
	}
	if why != "" {
		c.shadowUntil = nextMidnight(now)
		c.mu.Unlock()
		c.outage(ctx, why+"; the effective mode is at most shadow until midnight")
		return fmt.Errorf("%w: %s", ErrBudget, why)
	}
	c.calls = append(c.calls, now)
	c.mu.Unlock()
	return nil
}

func (c *Client) addTokens(now time.Time, n int64) {
	if c.cfg.Log != nil || n == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if day := midnight(now); !c.tokensDay.Equal(day) {
		c.tokens, c.tokensDay = 0, day
	}
	c.tokens += n
}

func (c *Client) fail(ctx context.Context, msg string) {
	now := c.cfg.Now()
	c.mu.Lock()
	c.failures++
	c.lastErr, c.lastErrAt = msg, now
	opened := false
	if c.failures >= CircuitFailures && !now.Before(c.openUntil) {
		c.openUntil, opened = now.Add(CircuitOpenFor), true
	}
	n := c.failures
	c.mu.Unlock()
	if opened {
		c.cfg.Logger.Warn("llm: circuit open", "failures", n, "for", CircuitOpenFor.String())
		c.outage(ctx, fmt.Sprintf("%d failures in a row (last: %s); the circuit is open for %s", n, msg, CircuitOpenFor))
	}
}

func (c *Client) succeed() {
	now := c.cfg.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures, c.openUntil = 0, time.Time{}
	if !now.Before(c.shadowUntil) {
		// The outage is over: the next one gets its own notice.
		c.noticed = false
	}
}

// outage stores the one passive notice of an outage.
func (c *Client) outage(ctx context.Context, why string) {
	c.mu.Lock()
	if c.noticed {
		c.mu.Unlock()
		return
	}
	c.noticed = true
	c.mu.Unlock()
	if c.cfg.Notify == nil {
		return
	}
	now := c.cfg.Now()
	fields := map[string]any{
		"source": NoticeSource, "kind": "message", "thread": NoticeThread, "level": "passive",
		"title": "Assistant unavailable", "summary": why, "tags": []any{"llm"},
		"dedup":   "llm-outage-" + ids.New(now),
		"details": map[string]any{"heraldo": map[string]any{"llm": map[string]any{"outage": why}}},
	}
	if err := c.cfg.Notify(context.WithoutCancel(ctx), fields); err != nil {
		c.cfg.Logger.Error("llm: outage notice", "err", err)
	}
}

func (c *Client) log(ctx context.Context, row store.LLMCall) string {
	if c.cfg.Log == nil {
		return ""
	}
	row.ID = "llm_" + ids.New(row.At)
	if err := c.cfg.Log.AddLLMCall(context.WithoutCancel(ctx), row); err != nil {
		c.cfg.Logger.Error("llm: call log", "err", err)
		return ""
	}
	c.prune(ctx, row.At)
	return row.ID
}

// prune drops call log rows older than KeepCalls, at most once an hour.
func (c *Client) prune(ctx context.Context, now time.Time) {
	p, ok := c.cfg.Log.(interface {
		PruneLLMCalls(ctx context.Context, before time.Time) (int64, error)
	})
	if !ok {
		return
	}
	c.mu.Lock()
	due := now.Sub(c.lastPrune) >= time.Hour
	if due {
		c.lastPrune = now
	}
	c.mu.Unlock()
	if !due {
		return
	}
	if _, err := p.PruneLLMCalls(context.WithoutCancel(ctx), now.Add(-KeepCalls)); err != nil {
		c.cfg.Logger.Error("llm: prune the call log", "err", err)
	}
}

// outputOf is what the call log keeps of a response: the content as JSON
// when it is a JSON object, else as a JSON string; tool calls by name and
// arguments. nil for an empty response.
func outputOf(r Response) json.RawMessage {
	if len(r.ToolCalls) > 0 {
		type tc struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		var calls []tc
		for _, t := range r.ToolCalls {
			calls = append(calls, tc{t.Name, t.Arguments})
		}
		b, _ := json.Marshal(map[string]any{"content": r.Content, "tool_calls": calls})
		return b
	}
	if r.Content == "" {
		return nil
	}
	t := strings.TrimSpace(r.Content)
	if strings.HasPrefix(t, "{") && json.Valid([]byte(t)) {
		return json.RawMessage(t)
	}
	b, _ := json.Marshal(r.Content)
	return b
}

// responseFromOutput reads back what outputOf wrote, for a cache hit.
func responseFromOutput(out json.RawMessage) Response {
	var s string
	if json.Unmarshal(out, &s) == nil {
		return Response{Content: s}
	}
	return Response{Content: string(out)}
}
