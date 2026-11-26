package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/delivery"
	"github.com/noahkiss/heraldo/internal/ids"
	"github.com/noahkiss/heraldo/internal/settings"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/validate"
)

// errStandbyDelivery is why a standby refuses a row or review write.
const errStandbyDelivery = "this server is a standby; it copies its delivery settings from the primary"

// SourceWindow is the window GET /v1/sources counts over.
const SourceWindow = 30 * 24 * time.Hour

func (s *server) deliveryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/delivery", s.auth(auth.ScopeRead, false, s.listDelivery))
	mux.HandleFunc("POST /v1/delivery", s.auth(auth.ScopeAdmin, false, s.notOnStandby(s.createDelivery)))
	mux.HandleFunc("POST /v1/delivery/preview", s.auth(auth.ScopeRead, false, s.previewDelivery))
	mux.HandleFunc("GET /v1/delivery/{id}", s.auth(auth.ScopeRead, false, s.getDelivery))
	mux.HandleFunc("PUT /v1/delivery/{id}", s.auth(auth.ScopeAdmin, false, s.notOnStandby(s.putDelivery)))
	mux.HandleFunc("DELETE /v1/delivery/{id}", s.auth(auth.ScopeAdmin, false, s.notOnStandby(s.deleteDelivery)))
	mux.HandleFunc("GET /v1/sources", s.auth(auth.ScopeRead, false, s.listSources))
	mux.HandleFunc("POST /v1/sources/{source}/review", s.auth(auth.ScopeAdmin, false, s.notOnStandby(s.reviewSource)))
	mux.HandleFunc("GET /v1/standby/delivery", s.auth(auth.ScopeRead, false, standbyOnly(s.exportDelivery)))
}

// notOnStandby answers 409 on a standby: its next round would overwrite
// the write with the primary's copy.
func (s *server) notOnStandby(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Standby {
			writeError(w, http.StatusConflict, errStandbyDelivery)
			return
		}
		h(w, r)
	}
}

// deliveryOn is delivery.enabled.
func (s *server) deliveryOn() bool { return settings.From[bool](s.Settings, "delivery.enabled") }

// deliveryConfig is what delivery.Resolve needs from the settings and the
// rules.
func (s *server) deliveryConfig() delivery.Config {
	set := s.Rules.Rules()
	return delivery.Config{
		NewSourceLevel: api.Level(settings.From[string](s.Settings, "delivery.new_source_level")),
		DigestOn:       settings.From[bool](s.Settings, "digest.enabled"),
		Alias:          set.Alias,
	}
}

// resolveDelivery runs the delivery settings on a typed input at ingest.
// rule is the ingest rule that typed it. It returns the input with the
// stamped level, validated again so the stored level column matches, and
// the stamp; nil without one. A lookup that fails leaves the event as it
// came: the event must never be lost to a delivery setting.
func (s *server) resolveDelivery(ctx context.Context, in *validate.Input, rule string) (*validate.Input, *api.DeliverStamp) {
	if !s.deliveryOn() {
		return in, nil
	}
	rows, err := s.Store.DeliveryRows(ctx)
	if err != nil {
		s.Logger.Error("delivery: rows; delivered as sent", "err", err)
		return in, nil
	}
	newSource, err := s.Store.SourceNew(ctx, in.Typed.Source)
	if err != nil {
		s.Logger.Error("delivery: sources; delivered as sent", "err", err)
		return in, nil
	}
	res := delivery.Resolve(delivery.FromInput(&in.Typed, rule), rows, newSource, s.deliveryConfig())
	if res.Stamp == nil {
		return in, nil
	}
	if res.SetLevel {
		f := maps.Clone(in.Fields)
		f["level"] = string(res.Level)
		out, err := s.Validator.Fields(f)
		if err != nil {
			s.Logger.Error("delivery: the stamped level is not valid; delivered as sent", "err", err)
			return in, nil
		}
		in = out
	}
	return in, res.Stamp
}

// ---- rows --------------------------------------------------------------

// DeliveryJSON is a row as the API serves it.
func DeliveryJSON(d delivery.Row) api.DeliverySetting {
	out := api.DeliverySetting{Id: d.ID, Match: delivery.MatchJSON(d.Match), Badge: api.DeliveryBadge(delivery.BadgeAuto),
		Position: d.Position, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt}
	if d.Badge != "" {
		out.Badge = api.DeliveryBadge(d.Badge)
	}
	if d.Deliver != "" {
		o := api.DeliverOutcome(d.Deliver)
		out.Deliver = &o
	}
	if d.DefaultLevel != "" {
		l := api.DeliveryDefaultLevel(d.DefaultLevel)
		out.DefaultLevel = &l
	}
	if d.Note != "" {
		out.Note = &d.Note
	}
	if d.UpdatedBy != "" {
		out.UpdatedBy = &d.UpdatedBy
	}
	return out
}

// DeliveryRow is a row from the API as the store keeps it: a standby's
// copy of its primary's row.
func DeliveryRow(d api.DeliverySetting) delivery.Row {
	out := delivery.Row{ID: d.Id, Match: delivery.MatchFrom(d.Match), Badge: delivery.Badge(d.Badge), Position: d.Position,
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt}
	if d.Deliver != nil {
		out.Deliver = delivery.Outcome(*d.Deliver)
	}
	if d.DefaultLevel != nil {
		out.DefaultLevel = api.Level(*d.DefaultLevel)
	}
	if d.Note != nil {
		out.Note = *d.Note
	}
	if d.UpdatedBy != nil {
		out.UpdatedBy = *d.UpdatedBy
	}
	return out
}

func (s *server) decodeDelivery(w http.ResponseWriter, r *http.Request) (delivery.Row, bool) {
	var in api.DeliverySettingInput
	dec := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "want a DeliverySettingInput: %v", err)
		return delivery.Row{}, false
	}
	d, err := delivery.RowFromInput(in)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return delivery.Row{}, false
	}
	return d, true
}

func (s *server) deliveryStoreErr(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no delivery setting with that id")
		return
	}
	s.Logger.Error(what, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// listDelivery is GET /v1/delivery.
func (s *server) listDelivery(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.DeliveryRows(r.Context())
	if err != nil {
		s.deliveryStoreErr(w, "delivery", err)
		return
	}
	out := api.DeliverySettingList{Enabled: s.deliveryOn(),
		NewSourceLevel: api.DeliveryDefaultLevel(settings.From[string](s.Settings, "delivery.new_source_level")),
		Settings:       make([]api.DeliverySetting, 0, len(rows))}
	for _, d := range rows {
		out.Settings = append(out.Settings, DeliveryJSON(d))
	}
	writeJSON(w, http.StatusOK, out)
}

// getDelivery is GET /v1/delivery/{id}.
func (s *server) getDelivery(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.GetDelivery(r.Context(), r.PathValue("id"))
	if err != nil {
		s.deliveryStoreErr(w, "delivery", err)
		return
	}
	writeJSON(w, http.StatusOK, DeliveryJSON(d))
}

// createDelivery is POST /v1/delivery.
func (s *server) createDelivery(w http.ResponseWriter, r *http.Request) {
	d, ok := s.decodeDelivery(w, r)
	if !ok {
		return
	}
	now := s.Now().UTC()
	d.ID, d.CreatedAt, d.UpdatedAt, d.UpdatedBy = "dlv_"+ids.New(now), now, now, tokenFrom(r).Name
	if err := s.Store.AddDelivery(r.Context(), d); err != nil {
		s.deliveryStoreErr(w, "add delivery", err)
		return
	}
	s.Logger.Info("delivery setting", "id", d.ID, "deliver", d.Deliver, "badge", d.Badge, "token", d.UpdatedBy)
	s.rebadge(r.Context())
	writeJSON(w, http.StatusCreated, DeliveryJSON(d))
}

// putDelivery is PUT /v1/delivery/{id}.
func (s *server) putDelivery(w http.ResponseWriter, r *http.Request) {
	old, err := s.Store.GetDelivery(r.Context(), r.PathValue("id"))
	if err != nil {
		s.deliveryStoreErr(w, "delivery", err)
		return
	}
	d, ok := s.decodeDelivery(w, r)
	if !ok {
		return
	}
	d.ID, d.CreatedAt, d.UpdatedAt, d.UpdatedBy = old.ID, old.CreatedAt, s.Now().UTC(), tokenFrom(r).Name
	if err := s.Store.PutDelivery(r.Context(), d); err != nil {
		s.deliveryStoreErr(w, "put delivery", err)
		return
	}
	s.Logger.Info("delivery setting", "id", d.ID, "deliver", d.Deliver, "badge", d.Badge, "token", d.UpdatedBy)
	s.rebadge(r.Context())
	writeJSON(w, http.StatusOK, DeliveryJSON(d))
}

// deleteDelivery is DELETE /v1/delivery/{id}.
func (s *server) deleteDelivery(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteDelivery(r.Context(), r.PathValue("id")); err != nil {
		s.deliveryStoreErr(w, "delete delivery", err)
		return
	}
	s.rebadge(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// rebadge re-stamps the badge of the unread events and the open incidents
// after a row change, so a change acts at once. A stamp is otherwise a
// snapshot.
func (s *server) rebadge(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	rows, err := s.Store.DeliveryRows(ctx)
	if err != nil {
		s.Logger.Error("delivery: re-stamp", "err", err)
		return
	}
	seq, found, err := s.Store.Cursor(ctx, inboxCursor)
	if err == nil && !found {
		seq, err = s.Store.Head(ctx)
	}
	if err != nil {
		s.Logger.Error("delivery: re-stamp", "err", err)
		return
	}
	cfg := s.deliveryConfig()
	n, err := s.Store.Rebadge(ctx, seq, func(ev *api.Event) string {
		return string(delivery.Resolve(delivery.FromStored(ev), rows, false, cfg).BadgeChoice())
	})
	if err != nil {
		s.Logger.Error("delivery: re-stamp", "err", err)
		return
	}
	if n > 0 {
		s.Logger.Info("delivery: badge re-stamped", "changed", n)
	}
}

// MirrorDelivery is a standby's copy of its primary's delivery state:
// the rows, the two scalar keys (a key locked by a flag or env stays as it
// is) and the sources' review state.
func (srv *Server) MirrorDelivery(ctx context.Context, d api.StandbyDelivery) error {
	s := srv.s
	rows := make([]delivery.Row, 0, len(d.Settings))
	for _, x := range d.Settings {
		rows = append(rows, DeliveryRow(x))
	}
	changed, err := s.Store.MirrorDelivery(ctx, rows)
	if err != nil {
		return fmt.Errorf("copy delivery settings: %w", err)
	}
	for name, v := range map[string]any{"delivery.enabled": d.Enabled, "delivery.new_source_level": string(d.NewSourceLevel)} {
		cur, err := s.Settings.Value(name)
		if err == nil && fmt.Sprint(cur) == fmt.Sprint(v) {
			continue
		}
		raw, _ := json.Marshal(v)
		if _, err := s.Settings.Set(ctx, name, raw, "standby"); err != nil {
			var le *settings.LockedError
			if errors.As(err, &le) {
				s.Logger.Warn("standby: a delivery key is locked here; kept as it is", "key", name, "by", le.By)
				continue
			}
			return fmt.Errorf("copy %s: %w", name, err)
		}
	}
	srcs := make([]store.Source, 0, len(d.Sources))
	for _, x := range d.Sources {
		y := store.Source{Source: x.Source, FirstSeen: x.FirstSeen, LastSeen: x.LastSeen}
		if x.ReviewedAt != nil {
			y.ReviewedAt = *x.ReviewedAt
		}
		srcs = append(srcs, y)
	}
	if err := s.Store.MirrorSources(ctx, srcs); err != nil {
		return fmt.Errorf("copy sources: %w", err)
	}
	if changed {
		s.rebadge(ctx)
	}
	return nil
}

// exportDelivery is GET /v1/standby/delivery.
func (s *server) exportDelivery(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.DeliveryRows(r.Context())
	if err != nil {
		s.deliveryStoreErr(w, "standby delivery", err)
		return
	}
	srcs, err := s.Store.Sources(r.Context())
	if err != nil {
		s.deliveryStoreErr(w, "standby delivery", err)
		return
	}
	out := api.StandbyDelivery{Enabled: s.deliveryOn(),
		NewSourceLevel: api.DeliveryDefaultLevel(settings.From[string](s.Settings, "delivery.new_source_level")),
		Settings:       make([]api.DeliverySetting, 0, len(rows)), Sources: make([]api.StandbySource, 0, len(srcs))}
	for _, d := range rows {
		out.Settings = append(out.Settings, DeliveryJSON(d))
	}
	for _, x := range srcs {
		y := api.StandbySource{Source: x.Source, FirstSeen: x.FirstSeen, LastSeen: x.LastSeen}
		if !x.ReviewedAt.IsZero() {
			t := x.ReviewedAt
			y.ReviewedAt = &t
		}
		out.Sources = append(out.Sources, y)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- sources -----------------------------------------------------------

func sourceJSON(x store.Source, c store.SourceCount) api.Source {
	out := api.Source{Source: x.Source, FirstSeen: x.FirstSeen, LastSeen: x.LastSeen, New: x.New(),
		Events: c.Events, BadgeIn: c.BadgeIn, BadgeOut: c.Events - c.BadgeIn}
	if !x.ReviewedAt.IsZero() {
		t := x.ReviewedAt
		out.ReviewedAt = &t
	}
	return out
}

// listSources is GET /v1/sources.
func (s *server) listSources(w http.ResponseWriter, r *http.Request) {
	srcs, err := s.Store.Sources(r.Context())
	if err != nil {
		s.deliveryStoreErr(w, "sources", err)
		return
	}
	scope, _ := s.badgeScope()
	counts, err := s.Store.SourceCounts(r.Context(), scope, s.Now().Add(-SourceWindow))
	if err != nil {
		s.deliveryStoreErr(w, "sources", err)
		return
	}
	out := api.SourceList{Sources: make([]api.Source, 0, len(srcs))}
	for _, x := range srcs {
		out.Sources = append(out.Sources, sourceJSON(x, counts[x.Source]))
	}
	writeJSON(w, http.StatusOK, out)
}

// reviewSource is POST /v1/sources/{source}/review.
func (s *server) reviewSource(w http.ResponseWriter, r *http.Request) {
	x, err := s.Store.ReviewSource(r.Context(), r.PathValue("source"), s.Now().UTC())
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no event from that source is stored")
		return
	}
	if err != nil {
		s.deliveryStoreErr(w, "review source", err)
		return
	}
	scope, _ := s.badgeScope()
	counts, err := s.Store.SourceCounts(r.Context(), scope, s.Now().Add(-SourceWindow))
	if err != nil {
		s.deliveryStoreErr(w, "review source", err)
		return
	}
	writeJSON(w, http.StatusOK, sourceJSON(x, counts[x.Source]))
}

// ---- preview -----------------------------------------------------------

// previewDelivery is POST /v1/delivery/preview.
func (s *server) previewDelivery(w http.ResponseWriter, r *http.Request) {
	var in api.DeliveryPreviewInput
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "want a DeliveryPreviewInput: %v", err)
		return
	}
	if len(in.Settings) > 200 {
		writeError(w, http.StatusBadRequest, "settings: at most 200 rows")
		return
	}
	rows := make([]delivery.Row, 0, len(in.Settings))
	for i, x := range in.Settings {
		d, err := delivery.RowFromInput(x)
		if err != nil {
			writeError(w, http.StatusBadRequest, "settings[%d]: %v", i, err)
			return
		}
		d.ID = fmt.Sprintf("row%d", i)
		rows = append(rows, d)
	}
	days, limit := 30, 200
	if in.Days != nil {
		days = *in.Days
	}
	if in.Limit != nil {
		limit = *in.Limit
	}
	if days < 1 || days > 90 {
		writeError(w, http.StatusBadRequest, "days: want 1 to 90")
		return
	}
	if limit < 0 || limit > 1000 {
		writeError(w, http.StatusBadRequest, "limit: want 0 to 1000")
		return
	}
	recs, err := s.Store.EventsSince(r.Context(), s.Now().Add(-time.Duration(days)*24*time.Hour))
	if err != nil {
		s.deliveryStoreErr(w, "preview", err)
		return
	}
	evs := make([]api.Event, 0, len(recs))
	for _, rec := range recs {
		var ev api.Event
		if json.Unmarshal(rec.Event, &ev) == nil {
			evs = append(evs, ev)
		}
	}
	scope, _ := s.badgeScope()
	p := Preview(evs, rows, scope, s.deliveryConfig(), limit)
	p.Days = days
	writeJSON(w, http.StatusOK, p)
}

// Preview runs rows over evs, newest first, each from its unstamped self,
// as if delivery.enabled were on and every source known. It lists at most
// limit changed events.
func Preview(evs []api.Event, rows []delivery.Row, scope string, cfg delivery.Config, limit int) api.DeliveryPreview {
	out := api.DeliveryPreview{Changed: []api.DeliveryPreviewEvent{}}
	out.Rows = make([]struct {
		Hits    int `json:"hits"`
		Index   int `json:"index"`
		Refused int `json:"refused"`
	}, len(rows))
	for i := range out.Rows {
		out.Rows[i].Index = i
	}
	for i := range evs {
		ev := &evs[i]
		e := delivery.FromStored(ev)
		if e.Routine() {
			continue
		}
		out.Events++
		before := e.Level
		if before == "" {
			before = api.LevelActive
		}
		res := delivery.Resolve(e, rows, false, cfg)
		badgeBefore := delivery.DefaultBadge(e, before, scope)
		badgeAfter := delivery.InBadge(e, res, scope)
		as := res.As()
		if res.Stamp == nil {
			out.Outcomes.Unchanged++
		} else {
			switch as {
			case delivery.Banner:
				out.Outcomes.Banner++
			case delivery.Silent:
				out.Outcomes.Silent++
			case delivery.Digest:
				out.Outcomes.Digest++
			case delivery.Drop:
				out.Outcomes.Drop++
			}
		}
		if res.Row >= 0 {
			out.Rows[res.Row].Hits++
			if res.Stamp.Refused != nil {
				out.Rows[res.Row].Refused++
			}
		}
		switch {
		case badgeAfter && !badgeBefore:
			out.BadgeJoined++
		case badgeBefore && !badgeAfter:
			out.BadgeLeft++
		}
		if as == delivery.Banner && res.Effective == before && badgeBefore == badgeAfter {
			continue
		}
		if len(out.Changed) >= limit {
			out.Truncated = true
			continue
		}
		c := api.DeliveryPreviewEvent{Id: ev.Id, Source: ev.Source, Title: ev.Title, Kind: ev.Kind,
			As: api.DeliverOutcome(as), LevelBefore: before, LevelAfter: res.Effective,
			BadgeBefore: badgeBefore, BadgeAfter: badgeAfter, Refused: nil}
		if res.Row >= 0 {
			n := res.Row
			c.Row = &n
		}
		if res.Stamp != nil {
			c.Refused = res.Stamp.Refused
		}
		out.Changed = append(out.Changed, c)
	}
	return out
}
