// Package standby runs a heraldo server as the standby of a primary, for
// when the primary's host is down. On every tick the standby:
//
//   - copies the primary's devices, full push tokens included, so its own
//     pusher reaches the same phones and Macs with its own copy of the APNs
//     key;
//   - hands every event it stored back to the primary, which imports them by
//     id and pushes none of them again;
//   - raises one "primary unreachable" incident once the primary has not
//     answered for DownAfter, and resolves it when the primary answers again.
//
// The standby accepts posts as any heraldo server does. It never needs the
// primary to reach it: every call goes from the standby to the primary.
package standby

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/server"
	"github.com/noahkiss/heraldo/internal/store"
)

// Defaults for Config.
const (
	DefaultInterval  = time.Minute
	DefaultDownAfter = 10 * time.Minute
)

// AlertKey is the incident key of the primary-down alert.
const AlertKey = "heraldo:primary"

// alertSource is the source of the primary-down alert.
const alertSource = "heraldo"

// batch is the number of events one import carries.
const batch = 100

// Names in the store's standby state.
const (
	stateExported = "exported_seq" // the last seq the primary imported
	stateDown     = "primary_down" // when the alert was raised; empty when none is open
)

// Config configures a Standby.
type Config struct {
	// PrimaryURL is the primary's root, such as https://heraldo.example.net.
	PrimaryURL string
	// Token is the standby's token on the primary: a read token with the
	// standby capability.
	Token string
	// HTTP is the client for the primary; nil means one with a 15 second
	// timeout.
	HTTP  *http.Client
	Store *store.Store
	// Emit stores and pushes an event, as the server's Emit does.
	Emit func(context.Context, map[string]any) error
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Interval is the gap between ticks; 0 means DefaultInterval.
	Interval time.Duration
	// DownAfter is how long the primary may go unanswered before the alert;
	// 0 means DefaultDownAfter.
	DownAfter time.Duration
	Logger    *slog.Logger
}

// Standby keeps a standby server in step with its primary.
type Standby struct {
	cfg Config
	// lastOK is when the primary last answered. A standby that starts
	// counts from its start.
	lastOK time.Time
}

// New returns a Standby. Start it with Run.
func New(cfg Config) *Standby {
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Interval == 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.DownAfter == 0 {
		cfg.DownAfter = DefaultDownAfter
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	cfg.PrimaryURL = strings.TrimRight(cfg.PrimaryURL, "/")
	return &Standby{cfg: cfg, lastOK: cfg.Now()}
}

// Run ticks until ctx ends, the first time at once.
func (s *Standby) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.cfg.Logger.Warn("standby", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// unreachable is a failure that means the primary is down: no answer, or a
// 5xx such as a proxy's 502.
type unreachable struct{ err error }

func (u unreachable) Error() string { return "primary unreachable: " + u.err.Error() }
func (u unreachable) Unwrap() error { return u.err }

// Tick does one round: copy the devices, settle the alert, hand back the
// events. Tests call it with a fake clock.
func (s *Standby) Tick(ctx context.Context) error {
	now := s.cfg.Now()
	var list api.StandbyDeviceList
	err := s.call(ctx, http.MethodGet, "/v1/standby/devices", nil, &list)
	var u unreachable
	if errors.As(err, &u) {
		return errors.Join(err, s.raise(ctx, now))
	}
	s.lastOK = now
	if err != nil {
		// The primary answered, so it is up; a refused token is a
		// configuration error, logged on every tick.
		return err
	}
	ds := make([]store.Device, 0, len(list.Devices))
	for _, d := range list.Devices {
		ds = append(ds, server.StandbyDevice(d, "standby"))
	}
	if err := s.cfg.Store.MirrorDevices(ctx, ds); err != nil {
		return fmt.Errorf("copy devices: %w", err)
	}
	if err := s.resolve(ctx, now); err != nil {
		return err
	}
	return s.handBack(ctx)
}

// raise opens the alert once the primary has been silent for DownAfter.
func (s *Standby) raise(ctx context.Context, now time.Time) error {
	if now.Sub(s.lastOK) < s.cfg.DownAfter {
		return nil
	}
	if v, err := s.cfg.Store.StandbyValue(ctx, stateDown); err != nil || v != "" {
		return err
	}
	err := s.cfg.Emit(ctx, map[string]any{
		"source": alertSource, "kind": "incident", "action": "open", "key": AlertKey, "level": "time-sensitive",
		"title": "heraldo primary unreachable",
		"summary": fmt.Sprintf("The standby has not reached the primary for %s. The standby delivers pushes until it is back.",
			roundDur(now.Sub(s.lastOK))),
	})
	if err != nil {
		return fmt.Errorf("raise the alert: %w", err)
	}
	s.cfg.Logger.Warn("standby: primary unreachable; alert raised", "since", s.lastOK)
	return s.cfg.Store.SetStandbyValue(ctx, stateDown, now.UTC().Format(time.RFC3339Nano))
}

// resolve resolves an open alert: the primary answered.
func (s *Standby) resolve(ctx context.Context, now time.Time) error {
	v, err := s.cfg.Store.StandbyValue(ctx, stateDown)
	if err != nil || v == "" {
		return err
	}
	f := map[string]any{
		"source": alertSource, "kind": "incident", "action": "resolve", "key": AlertKey, "level": "passive",
		"title": "heraldo primary reachable again",
	}
	if at, err := time.Parse(time.RFC3339Nano, v); err == nil {
		f["summary"] = fmt.Sprintf("The alert was open for %s.", roundDur(now.Sub(at)))
	}
	if err := s.cfg.Emit(ctx, f); err != nil {
		return fmt.Errorf("resolve the alert: %w", err)
	}
	s.cfg.Logger.Info("standby: primary reachable again; alert resolved")
	return s.cfg.Store.SetStandbyValue(ctx, stateDown, "")
}

// handBack imports every event stored since the last import into the
// primary, in batches, oldest first.
func (s *Standby) handBack(ctx context.Context) error {
	v, err := s.cfg.Store.StandbyValue(ctx, stateExported)
	if err != nil {
		return err
	}
	after, _ := strconv.ParseInt(v, 10, 64)
	for {
		recs, err := s.cfg.Store.List(ctx, store.Filter{AfterSeq: after, Limit: batch})
		if err != nil || len(recs) == 0 {
			return err
		}
		body := struct {
			Events []json.RawMessage `json:"events"`
		}{}
		for _, r := range recs {
			body.Events = append(body.Events, r.Event)
		}
		var res api.StandbyImportResult
		if err := s.call(ctx, http.MethodPost, "/v1/standby/import", body, &res); err != nil {
			return fmt.Errorf("hand back events: %w", err)
		}
		if res.Rejected > 0 {
			s.cfg.Logger.Warn("standby: the primary rejected events; see its log", "rejected", res.Rejected)
		}
		after = recs[len(recs)-1].Seq
		if err := s.cfg.Store.SetStandbyValue(ctx, stateExported, strconv.FormatInt(after, 10)); err != nil {
			return err
		}
		s.cfg.Logger.Info("standby: events handed back", "imported", res.Imported, "skipped", res.Skipped)
		if len(recs) < batch {
			return nil
		}
	}
}

// call sends one request to the primary and decodes a 2xx answer into out.
func (s *Standby) call(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.cfg.PrimaryURL+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.cfg.HTTP.Do(req)
	if err != nil {
		return unreachable{err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return unreachable{err}
	}
	if resp.StatusCode >= 500 {
		return unreachable{fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e api.Error
		_ = json.Unmarshal(raw, &e)
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, e.Error)
	}
	return json.Unmarshal(raw, out)
}

// roundDur is d to the minute, or to the second under a minute.
func roundDur(d time.Duration) time.Duration {
	if d < time.Minute {
		return d.Round(time.Second)
	}
	return d.Round(time.Minute)
}
