package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/checkin"
	"github.com/noahkiss/heraldo/internal/store"
)

// LogLimit is how much of a ping's body the server keeps as the log.
const LogLimit = 100_000

// checkinSource is the source of the incidents the scheduler raises.
const checkinSource = "heraldo"

// pingParams are the query parameters a ping takes.
var pingParams = map[string]bool{
	"rid": true, "create": true, "source": true, "host": true,
	"period": true, "cron": true, "tz": true, "grace": true, "max_runtime": true, "reminder": true,
}

// ---- ping API ----------------------------------------------------------

// ping is GET|POST /ping/{slug}[/start|/fail|/log|/{exit_code}], in the
// healthchecks.io shape, with a write token in the Authorization header.
func (s *server) ping(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !checkin.Slug.MatchString(slug) {
		writeError(w, http.StatusBadRequest, "slug: 1 to 200 of A-Z, a-z, 0-9, dot, underscore, colon and dash")
		return
	}
	s.recordPing(w, r, slug, false)
}

// pingKey is the form of a ping key: 24 random bytes, base64url.
var pingKey = regexp.MustCompile(`^[A-Za-z0-9_-]{32}$`)

// keyPing is GET|POST /p/{key}[/start|/fail|/log|/{exit_code}]. The key
// names one check and is the only credential: it can ping that check and
// nothing else, so a leaked cron line exposes one check.
func (s *server) keyPing(w http.ResponseWriter, r *http.Request) {
	k := r.PathValue("key")
	slug, found := "", false
	if pingKey.MatchString(k) {
		var err error
		slug, found, err = s.Store.CheckByPingKey(r.Context(), auth.Hash(k))
		if err != nil {
			s.Logger.Error("ping key", "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "unknown ping key")
		return
	}
	if who, _ := r.Context().Value(ctxKey{}).(*string); who != nil {
		*who = "ping-key:" + slug
	}
	s.recordPing(w, r, slug, true)
}

// recordPing stores one check-in event for slug; the body, cut to LogLimit,
// is its log. byKey is a ping key's ping, which may not create a check.
func (s *server) recordPing(w http.ResponseWriter, r *http.Request, slug string, byKey bool) {
	ctx := r.Context()
	var action string
	var exit *int
	switch op := r.PathValue("op"); op {
	case "":
		action = "success"
	case "start", "fail", "log":
		action = op
	default:
		n, err := strconv.Atoi(op)
		if err != nil || n < 0 || n > 255 || strconv.Itoa(n) != op {
			writeError(w, http.StatusNotFound, "want /ping/<slug>, or /start, /fail, /log or an exit code 0 to 255 after it")
			return
		}
		exit = &n
		action = "success"
		if n != 0 {
			action = "fail"
		}
	}
	q := r.URL.Query()
	for name, vals := range q {
		if !pingParams[name] {
			writeError(w, http.StatusBadRequest, "unknown parameter %q", name)
			return
		}
		if len(vals) != 1 {
			writeError(w, http.StatusBadRequest, "%s: give it once", name)
			return
		}
	}
	rid := q.Get("rid")
	if len(rid) > 64 {
		writeError(w, http.StatusBadRequest, "rid: at most 64 characters")
		return
	}
	log, truncated, err := readLog(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: %v", err)
		return
	}

	c, found, err := s.Store.LoadCheck(ctx, slug)
	if err != nil {
		s.Logger.Error("ping: check", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if byKey && q.Has("create") {
		writeError(w, http.StatusBadRequest, "create: a ping key pings a known check; create checks with a write token")
		return
	}
	if !found {
		if q.Get("create") != "1" {
			writeError(w, http.StatusNotFound, "no check %q; add ?create=1 to create it", slug)
			return
		}
		sched, err := scheduleFromQuery(q.Get)
		if err != nil {
			writeError(w, http.StatusBadRequest, "%v", err)
			return
		}
		if _, err := s.Store.PutCheck(ctx, slug, q.Get("host"), sched); err != nil {
			s.Logger.Error("ping: create", "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	source := q.Get("source")
	if source == "" {
		source = "checkin"
	}
	f := map[string]any{"source": source, "kind": "checkin", "key": slug, "action": action}
	if h := q.Get("host"); h != "" {
		f["host"] = h
	}
	ck := map[string]any{}
	if rid != "" {
		ck["rid"] = rid
	}
	if exit != nil {
		ck["exit_code"] = *exit
	}
	if len(ck) > 0 {
		f["checkin"] = ck
	}
	if log != "" {
		f["body"] = log
	}
	if truncated {
		f["details"] = map[string]any{"heraldo": map[string]any{"log_truncated": true}}
	}
	if action == "fail" {
		f["title"] = slug + " failed"
		if exit != nil {
			f["title"] = fmt.Sprintf("%s failed (exit %d)", slug, *exit)
		}
		// A paused check is in maintenance: its failures stay quiet.
		if c.State == checkin.StatePaused {
			f["level"] = "passive"
		}
	}
	in, err := s.Validator.Fields(f)
	if err != nil {
		s.badInput(w, err)
		return
	}
	if _, _, err := s.ingestPing(ctx, in); err != nil {
		writeError(w, http.StatusInternalServerError, "could not store the ping")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "OK")
}

// readLog reads a ping's body, keeping the first LogLimit bytes, cut at a
// character boundary. The rest is read and dropped.
func readLog(body io.Reader) (string, bool, error) {
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(body, LogLimit+1))
	if err != nil {
		return "", false, err
	}
	if n <= LogLimit {
		return buf.String(), false, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, MaxRequestBytes))
	b := buf.Bytes()[:LogLimit]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b), true, nil
}

// scheduleFromQuery reads a new check's schedule from ping parameters:
// period, cron, tz, grace, max_runtime and reminder. A duration is seconds or
// a Go duration such as 1h30m.
func scheduleFromQuery(get func(string) string) (checkin.Schedule, error) {
	sched := store.ScheduleFrom(nil)
	sched.Cron, sched.TZ = get("cron"), get("tz")
	for _, d := range []struct {
		name string
		dst  *time.Duration
	}{
		{"period", &sched.Period}, {"grace", &sched.Grace}, {"max_runtime", &sched.MaxRuntime}, {"reminder", &sched.Reminder},
	} {
		v := get(d.name)
		if v == "" {
			continue
		}
		dur, err := parseDuration(v)
		if err != nil {
			return sched, fmt.Errorf("%s: %v", d.name, err)
		}
		*d.dst = dur
	}
	return sched, sched.Validate()
}

func parseDuration(v string) (time.Duration, error) {
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 {
			return 0, errors.New("may not be negative")
		}
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%q is not seconds or a duration such as 1h30m", v)
	}
	if d < 0 {
		return 0, errors.New("may not be negative")
	}
	return d, nil
}

// newPingKey returns a new ping key.
func newPingKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// mintPingKey is POST /v1/checkins/{slug}/key: a new ping key for the
// check, which replaces the old one. The answer is the only time the server
// shows the key; it keeps the hash.
func (s *server) mintPingKey(w http.ResponseWriter, r *http.Request) {
	slug, ok := s.slug(w, r)
	if !ok {
		return
	}
	k, err := newPingKey()
	if err == nil {
		err = s.Store.SetPingKey(r.Context(), slug, auth.Hash(k))
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no check %q", slug)
		return
	}
	if err != nil {
		s.Logger.Error("ping key", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.Logger.Info("ping key minted", "check", slug, "by", tokenFrom(r).Name)
	writeJSON(w, http.StatusCreated, api.PingKey{Key: k, Path: "/p/" + k})
}

// removePingKey is DELETE /v1/checkins/{slug}/key.
func (s *server) removePingKey(w http.ResponseWriter, r *http.Request) {
	slug, ok := s.slug(w, r)
	if !ok {
		return
	}
	err := s.Store.SetPingKey(r.Context(), slug, "")
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no check %q", slug)
		return
	}
	if err != nil {
		s.Logger.Error("ping key", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.Logger.Info("ping key removed", "check", slug, "by", tokenFrom(r).Name)
	s.checkAnswer(w, r, slug, http.StatusOK)
}

// ---- checks API --------------------------------------------------------

func checkJSON(v store.CheckView) api.Check {
	c := v.Check
	out := api.Check{
		Key: c.Key, State: api.CheckState(c.State), StateSince: c.StateSince.UTC(), CreatedAt: c.CreatedAt.UTC(),
		Host: strPtr(c.Host), LastEvent: strPtr(c.LastEvent), LastLog: strPtr(v.LastLog),
		LastPing: timePtr(c.LastPing), LastSuccess: timePtr(c.LastSuccess), StartedAt: timePtr(c.StartedAt),
		NextDue: timePtr(c.DueAt), DownAfter: timePtr(c.DownAfter()), LastLogAt: timePtr(v.LastLogAt),
		PingKeyAt: timePtr(c.PingKeyAt),
	}
	if c.LastLogEvent == "" {
		out.LastLogAt = nil
	}
	if c.DownReason != "" {
		r := api.CheckDownReason(c.DownReason)
		out.DownReason = &r
	}
	if v.Incident != "" {
		i := api.CheckIncident(v.Incident)
		out.Incident = &i
	}
	if c.LastDuration >= 0 {
		ms := int(c.LastDuration.Milliseconds())
		out.LastDurationMs = &ms
	}
	sc := c.Schedule
	out.Schedule = api.CheckSchedule{
		Cron: strPtr(sc.Cron), Tz: strPtr(sc.TZ), PeriodS: secPtr(sc.Period), MaxRuntimeS: secPtr(sc.MaxRuntime),
	}
	g, rm := int(sc.Grace/time.Second), int(sc.Reminder/time.Second)
	out.Schedule.GraceS, out.Schedule.ReminderS = &g, &rm
	return out
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func secPtr(d time.Duration) *int {
	if d <= 0 {
		return nil
	}
	n := int(d / time.Second)
	return &n
}

func (s *server) listChecks(w http.ResponseWriter, r *http.Request) {
	vs, err := s.Store.Checks(r.Context())
	if err != nil {
		s.Logger.Error("checks", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := api.CheckList{Checkins: make([]api.Check, 0, len(vs))}
	for _, v := range vs {
		out.Checkins = append(out.Checkins, checkJSON(v))
	}
	writeJSON(w, http.StatusOK, out)
}

// checkAnswer writes the check called slug.
func (s *server) checkAnswer(w http.ResponseWriter, r *http.Request, slug string, status int) {
	v, err := s.Store.Check(r.Context(), slug)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no check %q", slug)
		return
	}
	if err != nil {
		s.Logger.Error("check", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, status, checkJSON(v))
}

func (s *server) slug(w http.ResponseWriter, r *http.Request) (string, bool) {
	slug := r.PathValue("slug")
	if !checkin.Slug.MatchString(slug) {
		writeError(w, http.StatusBadRequest, "slug: 1 to 200 of A-Z, a-z, 0-9, dot, underscore, colon and dash")
		return "", false
	}
	return slug, true
}

func (s *server) getCheck(w http.ResponseWriter, r *http.Request) {
	if slug, ok := s.slug(w, r); ok {
		s.checkAnswer(w, r, slug, http.StatusOK)
	}
}

// putCheck is PUT /v1/checkins/{slug} with a CheckConfig.
func (s *server) putCheck(w http.ResponseWriter, r *http.Request) {
	slug, ok := s.slug(w, r)
	if !ok {
		return
	}
	var cfg api.CheckConfig
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "want a CheckConfig: %v", err)
		return
	}
	sched := store.ScheduleFrom(&api.Schedule{
		PeriodS: cfg.PeriodS, Cron: cfg.Cron, Tz: cfg.Tz, GraceS: cfg.GraceS, MaxRuntimeS: cfg.MaxRuntimeS,
	})
	if cfg.ReminderS != nil {
		sched.Reminder = time.Duration(*cfg.ReminderS) * time.Second
	}
	if (cfg.PeriodS != nil && *cfg.PeriodS < 1) || (cfg.MaxRuntimeS != nil && *cfg.MaxRuntimeS < 1) {
		writeError(w, http.StatusBadRequest, "period_s and max_runtime_s: at least 1")
		return
	}
	if err := sched.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	host := ""
	if cfg.Host != nil {
		host = *cfg.Host
		if len(host) > 128 {
			writeError(w, http.StatusBadRequest, "host: at most 128 characters")
			return
		}
	}
	created, err := s.Store.PutCheck(r.Context(), slug, host, sched)
	if err != nil {
		s.Logger.Error("put check", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.judgeCheck(r.Context(), slug)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.checkAnswer(w, r, slug, status)
}

func (s *server) pauseCheck(w http.ResponseWriter, r *http.Request)  { s.setPaused(w, r, true) }
func (s *server) resumeCheck(w http.ResponseWriter, r *http.Request) { s.setPaused(w, r, false) }

func (s *server) setPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	slug, ok := s.slug(w, r)
	if !ok {
		return
	}
	err := s.Store.SetPaused(r.Context(), slug, paused)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no check %q", slug)
		return
	}
	if err != nil {
		s.Logger.Error("pause", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.judgeCheck(r.Context(), slug)
	s.checkAnswer(w, r, slug, http.StatusOK)
}

// ---- scheduler ---------------------------------------------------------

// RunCheckins judges every check each interval until ctx ends: late, down,
// incidents and reminders. All its state is in the database, so a restart
// picks up where it left off.
func (srv *Server) RunCheckins(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		srv.s.tickCheckins(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *server) tickCheckins(ctx context.Context) {
	cs, err := s.Store.JudgedChecks(ctx)
	if err != nil {
		s.Logger.Error("checkins: list", "err", err)
		return
	}
	for _, c := range cs {
		if ctx.Err() != nil {
			return
		}
		s.judgeCheck(ctx, c.Key)
	}
}

// judgeCheck applies the time to one check, then opens, reminds or resolves
// its incident, checkin:<key>, to match its state:
//
//   - down for a missed ping or an overrun opens it (level active);
//   - down for a failed run opens it (level passive: the failure itself
//     already pushed) when the check is scheduled, so a success will come
//     to resolve it. An unscheduled check's failure is its own alert;
//   - up, new or paused resolves it;
//   - while it stays open and unacked, a reminder every reminder_s.
func (s *server) judgeCheck(ctx context.Context, key string) {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	now := s.Now().UTC()
	c, found, err := s.Store.LoadCheck(ctx, key)
	if err != nil || !found {
		if err != nil {
			s.Logger.Error("checkins: load", "key", key, "err", err)
		}
		return
	}
	was := c
	if c.Tick(now, s.started) {
		ok, err := s.Store.SaveTicked(ctx, was, c)
		if err != nil {
			s.Logger.Error("checkins: save", "key", key, "err", err)
			return
		}
		if !ok {
			// A ping changed the check meanwhile; the next tick sees it.
			return
		}
	}
	incKey := checkin.IncidentPrefix + key
	inc, err := s.Store.IncidentState(ctx, incKey)
	if err != nil {
		s.Logger.Error("checkins: incident", "key", key, "err", err)
		return
	}
	open := inc == "open" || inc == "acked"
	f := map[string]any{
		"source": checkinSource, "kind": "incident", "key": incKey,
		"details": map[string]any{"heraldo": map[string]any{"check": key, "state": c.State, "reason": c.DownReason}},
	}
	if c.Host != "" {
		f["host"] = c.Host
	}
	remind := false
	switch {
	case open && (c.State == checkin.StateUp || c.State == checkin.StateNew || c.State == checkin.StatePaused):
		f["action"], f["level"] = "resolve", "passive"
		f["title"] = key + " is up"
		if c.State == checkin.StatePaused {
			f["title"] = key + " is paused"
		}
	case !open && c.State == checkin.StateDown && (c.DownReason != checkin.ReasonFailed || c.Schedule.Scheduled()):
		f["action"], f["level"] = "open", "active"
		f["title"], f["summary"] = downText(c)
		if c.DownReason == checkin.ReasonFailed {
			f["level"] = "passive"
		}
		remind = true
	case inc == "open" && c.State == checkin.StateDown && c.Schedule.Reminder > 0 &&
		!now.Before(c.RemindedAt.Add(c.Schedule.Reminder)):
		f["action"], f["level"] = "update", "active"
		_, summary := downText(c)
		f["title"] = key + " is still down"
		f["summary"] = cut(fmt.Sprintf("Down since %s. %s", stamp(c.StateSince), summary), 178)
		remind = true
	default:
		return
	}
	in, err := s.Validator.Fields(f)
	if err != nil {
		s.Logger.Error("checkins: incident event", "key", key, "err", err)
		return
	}
	if _, _, err := s.ingest(ctx, in, false); err != nil {
		return
	}
	if remind {
		if err := s.Store.SetReminded(ctx, key, now); err != nil {
			s.Logger.Error("checkins: reminded", "key", key, "err", err)
		}
	}
}

// downText is the title and summary of a down check's incident.
func downText(c checkin.Check) (string, string) {
	switch c.DownReason {
	case checkin.ReasonOverran:
		return c.Key + " is running too long",
			cut(fmt.Sprintf("Started %s; the limit is %s.", stamp(c.StartedAt), c.Schedule.MaxRuntime), 178)
	case checkin.ReasonFailed:
		return c.Key + " failed", cut(fmt.Sprintf("The run at %s failed.", stamp(c.LastPing)), 178)
	}
	if c.State == checkin.StateDown && !c.StartedAt.IsZero() {
		return c.Key + " is down",
			cut(fmt.Sprintf("A run started %s and has not ended; the next run was due after it, with %s of grace.", stamp(c.StartedAt), c.Schedule.Grace), 178)
	}
	last := "No ping yet."
	if !c.LastPing.IsZero() {
		last = "Last ping " + stamp(c.LastPing) + "."
	}
	return c.Key + " is down",
		cut(fmt.Sprintf("%s Due %s, with %s of grace.", last, stamp(c.DueAt), c.Schedule.Grace), 178)
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }
