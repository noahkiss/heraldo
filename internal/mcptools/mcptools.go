// Package mcptools is heraldo's MCP tool layer: the tools over the heraldo
// API, for coding agents. It knows nothing of the transport. heraldo mcp
// serves it on stdio; internal/mcphttp serves it on the server at
// POST /v1/mcp.
package mcptools

import (
	"context"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/client"
)

// API is the part of *client.Client the tools call.
type API interface {
	Health(ctx context.Context) (api.Health, error)
	List(ctx context.Context, q client.Query) (client.Page, error)
	Get(ctx context.Context, id string) (client.Event, error)
	Why(ctx context.Context, id string) (api.PushTrace, error)
	StateIn(ctx context.Context, projects []string) (client.State, error)
	CursorUnread(ctx context.Context, name string) (client.CursorState, bool, error)
	ChecksIn(ctx context.Context, projects []string) ([]client.CheckEntry, error)
	Check(ctx context.Context, slug string) (api.Check, error)
	PutCheck(ctx context.Context, slug string, cfg api.CheckConfig) (api.Check, error)
	SetPaused(ctx context.Context, slug string, paused bool) (api.Check, error)
	Activities(ctx context.Context, all bool) ([]api.Activity, error)
	Activity(ctx context.Context, key string) (api.Activity, error)
	Mutes(ctx context.Context) ([]api.Mute, error)
	Mute(ctx context.Context, in api.MuteInput) (api.Mute, error)
	EndMute(ctx context.Context, id string) (api.Mute, error)
	PostNew(ctx context.Context, input map[string]any) (client.Event, bool, error)
	Settings(ctx context.Context) ([]api.Setting, error)
	Setting(ctx context.Context, key string) (api.Setting, error)
	PutSetting(ctx context.Context, key string, value any) (api.Setting, error)
	DeleteSetting(ctx context.Context, key string) (api.Setting, error)
	UploadAttachment(ctx context.Context, contentType string, data []byte) (api.AttachmentCreated, error)
	Attachment(ctx context.Context, id string) ([]byte, string, error)
	Projects(ctx context.Context) (api.ProjectList, error)
	Devices(ctx context.Context) ([]api.Device, error)
	Hold(ctx context.Context, q client.HoldQuery) (api.HoldQueue, error)
	DigestPreview(ctx context.Context) (api.DigestPreview, error)
	HoldRelease(ctx context.Context, in api.HoldReleaseJSONBody) (client.HoldResult, error)
	HoldSuppress(ctx context.Context, in api.HoldSuppressJSONBody) (client.HoldResult, error)
	Views(ctx context.Context) ([]api.View, error)
	View(ctx context.Context, ref string) (api.View, error)
	SaveView(ctx context.Context, in api.ViewInput) (api.View, error)
	PutView(ctx context.Context, ref string, in api.ViewInput) (api.View, error)
	DeleteView(ctx context.Context, ref string) error
	Guidance(ctx context.Context, scope string) ([]api.Guidance, error)
	AddGuidance(ctx context.Context, in api.GuidanceInput) (api.Guidance, error)
	PutGuidance(ctx context.Context, id string, in api.GuidancePatch) (api.Guidance, error)
	DeleteGuidance(ctx context.Context, id string) error
	LLMStatus(ctx context.Context) (api.LlmStatus, error)
	Ask(ctx context.Context, in api.AskInput) (api.AskAnswer, error)
}

var _ API = (*client.Client)(nil)

// Options configure the tools.
type Options struct {
	// API is the heraldo server. nil means it is not configured: only
	// overview is listed, and it reports NotConfigured.
	API API
	// NotConfigured says what is missing, such as "HERALDO_URL is not set".
	NotConfigured string
	// ConfigHint says where settings come from, such as the env file path.
	ConfigHint string
	// Write registers the write tools. WriteOff says why it is false.
	Write    bool
	WriteOff string
	// Admin registers the admin tools: the API client has an admin token.
	Admin bool
	// URLHost is the host of the server URL, for overview. Never a token.
	URLHost string
	// Cursor is the read cursor that unread reads. Empty means inbox.
	Cursor string
	// Source is the source of the events the tools post. Empty means one
	// derived from the client: agent:<harness>.
	Source string
	// Harness names the harness when the client does not, such as claude.
	Harness string
	// Project is send's default project. Empty means none.
	Project string
	// Host is send's default host: this machine's short name.
	Host string
	// Version is this program's version.
	Version string
	Logger  *slog.Logger
}

type tools struct {
	Options
}

// New returns an MCP server with the tools Options allow.
func New(o Options) *mcp.Server {
	if o.Cursor == "" {
		o.Cursor = "inbox"
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	t := &tools{o}
	s := mcp.NewServer(&mcp.Implementation{Name: "heraldo", Version: o.Version},
		&mcp.ServerOptions{Instructions: t.instructions(), Logger: o.Logger})
	t.addOverview(s)
	if o.API == nil {
		return s
	}
	t.addEventsSearch(s)
	t.addEventRead(s)
	t.addCheckinRead(s)
	t.addActivityRead(s)
	t.addSettingsRead(s)
	t.addAttachmentRead(s)
	t.addHoldRead(s)
	t.addViewsRead(s)
	t.addDigestPreview(s)
	t.addGuidanceRead(s)
	t.addLLMStatus(s)
	t.addAsk(s)
	if o.Write {
		t.addIncidentAct(s)
		t.addCheckinUpdate(s)
		t.addMute(s)
		t.addSend(s)
		t.addHoldAct(s)
		t.addViewsWrite(s)
		t.addGuidanceWrite(s)
	}
	if o.Admin {
		t.addSettingsWrite(s)
	}
	return s
}

// ReadTools, WriteTools and AdminTools name the tools New registers.
var (
	ReadTools  = []string{"overview", "events_search", "event_read", "checkin_read", "activity_read", "settings_read", "attachment_read", "hold_read", "views_read", "digest_preview", "guidance_read", "llm_status", "ask"}
	WriteTools = []string{"incident_act", "checkin_update", "mute", "send", "hold_act", "views_write", "guidance_write"}
	AdminTools = []string{"settings_write"}
)

const instructionsRead = `heraldo is the owner's notification hub. Every alert, incident, check-in and long-running job lands here and is pushed to their phone and Mac.

Start with overview: it shows unread events, open incidents, checks that are down, running Live Activities and mutes. Use events_search for history and search, then event_read for one event's full text and its attachments, and attachment_read for one attached file.`

const instructionsWrite = `Writes reach the owner's devices. Act on incidents, checks and mutes when the owner asks, or when you fixed the cause and say so in the note. send pushes a notification: use level passive for routine news, active when the owner should look, time-sensitive only when they must act now. Set dedup on every send so a retry stores nothing twice.`

const instructionsIDs = `Event ids are ULIDs. Incident and progress events share a key; events_search key=<key> gives one incident's or one job's history. A check's incident key is checkin:<slug>.`

const instructionsHold = `The alert hold keeps alerts off the owner's phone until triage reads them. hold_read shows it; hold_act closes entries with one diagnosis. hold_read is not a triage poll unless you pass poll=true.`

func (t *tools) instructions() string {
	parts := []string{instructionsRead}
	if t.Write && t.API != nil {
		parts = append(parts, instructionsWrite)
	} else {
		parts = append(parts, "This server is read-only: it has no write token, so it cannot ack, mute, send or change checks.")
	}
	parts = append(parts, instructionsIDs, instructionsHold)
	return strings.Join(parts, "\n\n")
}

// source is the source of an event the tools post.
func (t *tools) source(req *mcp.CallToolRequest) string {
	if t.Source != "" {
		return t.Source
	}
	name := ""
	if req != nil {
		if ci := req.ClientInfo(); ci != nil {
			name = harnessFromClient(ci.Name)
		}
	}
	if name == "" {
		name = t.Harness
	}
	if name == "" {
		return "heraldo-mcp"
	}
	return "agent:" + name
}

// harnessFromClient maps an MCP client name to a short harness name.
func harnessFromClient(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, h := range []struct{ match, name string }{
		{"claude", "claude"}, {"codex", "codex"}, {"opencode", "opencode"}, {"copilot", "copilot"},
		{"cursor", "cursor"}, {"gemini", "gemini"},
	} {
		if strings.Contains(n, h.match) {
			return h.name
		}
	}
	if n == "pi" || strings.HasPrefix(n, "pi-") || strings.HasPrefix(n, "pi ") {
		return "pi"
	}
	var b strings.Builder
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == ' ' || r == '/':
			b.WriteByte('-')
		}
		if b.Len() >= 48 {
			break
		}
	}
	return b.String()
}

// HarnessFromEnv names the coding harness that started this process, from
// the variables each one sets for its children. Empty when none matches.
func HarnessFromEnv(getenv func(string) string) string {
	for _, h := range []struct{ env, name string }{
		{"CLAUDECODE", "claude"}, {"OPENCODE", "opencode"},
	} {
		if getenv(h.env) != "" {
			return h.name
		}
	}
	return ""
}

func ptr[T any](v T) *T { return &v }
