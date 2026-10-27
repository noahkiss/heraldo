package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ErrTokenTaken is returned by UpdateDevice when another device holds the
// token.
var ErrTokenTaken = errors.New("another device holds that token")

// Device is one registered device.
type Device struct {
	ID          string
	Token       string
	Environment string
	Topic       string
	Name        string
	Platform    string
	AppVersion  string
	// Permission health: nil is unknown.
	Alerts         *bool
	TimeSensitive  *bool
	LiveActivities *bool
	RegisteredAt   time.Time
	UpdatedAt      time.Time
	UpdatedBy      string
	// RetiredAt is zero for an active device.
	RetiredAt     time.Time
	RetiredReason string
	// LiveActivityToken is the push-to-start token for Live Activities;
	// empty when the app has not reported one.
	LiveActivityToken string
}

// Active reports whether the device gets pushes.
func (d Device) Active() bool { return d.RetiredAt.IsZero() }

const deviceCols = `id, token, environment, topic, name, platform, app_version, alerts, time_sensitive,
	live_activities, registered_at, updated_at, updated_by, retired_at, retired_reason, live_activity_token`

type scanner interface{ Scan(...any) error }

func scanDevice(r scanner) (Device, error) {
	var d Device
	var name, platform, appVersion, retiredAt, retiredReason, laToken sql.NullString
	var alerts, ts, la sql.NullBool
	var registered, updated string
	if err := r.Scan(&d.ID, &d.Token, &d.Environment, &d.Topic, &name, &platform, &appVersion,
		&alerts, &ts, &la, &registered, &updated, &d.UpdatedBy, &retiredAt, &retiredReason, &laToken); err != nil {
		return Device{}, err
	}
	d.Name, d.Platform, d.AppVersion = name.String, platform.String, appVersion.String
	d.RetiredReason = retiredReason.String
	d.LiveActivityToken = laToken.String
	d.Alerts, d.TimeSensitive, d.LiveActivities = boolPtr(alerts), boolPtr(ts), boolPtr(la)
	d.RegisteredAt, _ = time.Parse(timeFormat, registered)
	d.UpdatedAt, _ = time.Parse(timeFormat, updated)
	if retiredAt.Valid {
		d.RetiredAt, _ = time.Parse(timeFormat, retiredAt.String)
	}
	return d, nil
}

func boolPtr(b sql.NullBool) *bool {
	if !b.Valid {
		return nil
	}
	v := b.Bool
	return &v
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// GetDevice returns the device with id, or ErrNotFound.
func (s *Store) GetDevice(ctx context.Context, id string) (Device, error) {
	d, err := scanDevice(s.db.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	return d, err
}

// Devices lists the devices in registration order. active keeps those that
// are not retired.
func (s *Store) Devices(ctx context.Context, active bool) ([]Device, error) {
	q := `SELECT ` + deviceCols + ` FROM devices`
	if active {
		q += ` WHERE retired_at IS NULL`
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY registered_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RegisterDevice stores d by its token: a known token updates that device
// and makes it active again, an unknown one creates a device with id newID.
// It returns the stored device, the device as it was before (nil when
// created), and whether it created one.
func (s *Store) RegisterDevice(ctx context.Context, d Device, newID string) (Device, *Device, bool, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Device{}, nil, false, err
	}
	defer tx.Rollback()
	old, err := scanDevice(tx.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE token = ?`, d.Token))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		d.ID = newID
		if err := s.insertDevice(ctx, tx, d); err != nil {
			return Device{}, nil, false, err
		}
		got, err := s.commitDevice(ctx, tx, d.ID)
		return got, nil, true, err
	case err != nil:
		return Device{}, nil, false, err
	}
	d.ID = old.ID
	if err := s.updateDevice(ctx, tx, d); err != nil {
		return Device{}, nil, false, err
	}
	got, err := s.commitDevice(ctx, tx, d.ID)
	return got, &old, false, err
}

// UpdateDevice replaces the device d.ID, token included, and makes it active
// again. It returns the stored device and the device as it was before, or
// ErrNotFound, or ErrTokenTaken.
func (s *Store) UpdateDevice(ctx context.Context, d Device) (Device, Device, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Device{}, Device{}, err
	}
	defer tx.Rollback()
	old, err := scanDevice(tx.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE id = ?`, d.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, Device{}, ErrNotFound
	}
	if err != nil {
		return Device{}, Device{}, err
	}
	var other string
	err = tx.QueryRowContext(ctx, `SELECT id FROM devices WHERE token = ? AND id != ?`, d.Token, d.ID).Scan(&other)
	if err == nil {
		return Device{}, Device{}, ErrTokenTaken
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Device{}, Device{}, err
	}
	if err := s.updateDevice(ctx, tx, d); err != nil {
		return Device{}, Device{}, err
	}
	got, err := s.commitDevice(ctx, tx, d.ID)
	return got, old, err
}

func (s *Store) insertDevice(ctx context.Context, tx *sql.Tx, d Device) error {
	at := s.now().UTC().Format(timeFormat)
	_, err := tx.ExecContext(ctx, `INSERT INTO devices (`+deviceCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?)`,
		d.ID, d.Token, d.Environment, d.Topic, nullStr(d.Name), nullStr(d.Platform), nullStr(d.AppVersion),
		d.Alerts, d.TimeSensitive, d.LiveActivities, at, at, d.UpdatedBy, nullStr(d.LiveActivityToken))
	return err
}

func (s *Store) updateDevice(ctx context.Context, tx *sql.Tx, d Device) error {
	at := s.now().UTC().Format(timeFormat)
	_, err := tx.ExecContext(ctx, `UPDATE devices SET token = ?, environment = ?, topic = ?, name = ?,
		platform = ?, app_version = ?, alerts = ?, time_sensitive = ?, live_activities = ?, updated_at = ?,
		updated_by = ?, retired_at = NULL, retired_reason = NULL, live_activity_token = ? WHERE id = ?`,
		d.Token, d.Environment, d.Topic, nullStr(d.Name), nullStr(d.Platform), nullStr(d.AppVersion),
		d.Alerts, d.TimeSensitive, d.LiveActivities, at, d.UpdatedBy, nullStr(d.LiveActivityToken), d.ID)
	return err
}

func (s *Store) commitDevice(ctx context.Context, tx *sql.Tx, id string) (Device, error) {
	d, err := scanDevice(tx.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE id = ?`, id))
	if err != nil {
		return Device{}, err
	}
	return d, tx.Commit()
}

// RetireDevice stops pushes to the device id and records why. Retiring a
// retired device keeps the first reason. It returns ErrNotFound for an
// unknown id.
func (s *Store) RetireDevice(ctx context.Context, id, reason string) (Device, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	at := s.now().UTC().Format(timeFormat)
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET retired_at = coalesce(retired_at, ?),
		retired_reason = coalesce(retired_reason, ?) WHERE id = ?`, at, reason, id)
	if err != nil {
		return Device{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Device{}, ErrNotFound
	}
	return s.GetDevice(ctx, id)
}

// RetireToken retires the device with token, if it still holds it. A device
// that registered a new token since the push went out is left alone.
func (s *Store) RetireToken(ctx context.Context, id, token, reason string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	at := s.now().UTC().Format(timeFormat)
	_, err := s.db.ExecContext(ctx, `UPDATE devices SET retired_at = ?, retired_reason = ?
		WHERE id = ? AND token = ? AND retired_at IS NULL`, at, reason, id, token)
	return err
}

// Delivery is the push outcome for one event on one device.
type Delivery struct {
	EventID  string
	DeviceID string
	Outcome  string
	Level    string
	At       time.Time
	Error    string
	APNsID   string
	Attempts int
}

// RecordDelivery stores the outcome of a push. A later outcome for the same
// event and device replaces the earlier one.
func (s *Store) RecordDelivery(ctx context.Context, d Delivery) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO deliveries (event_id, device_id, outcome, level, at, error, apns_id, attempts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (event_id, device_id) DO UPDATE SET outcome = excluded.outcome, level = excluded.level,
		at = excluded.at, error = excluded.error, apns_id = excluded.apns_id, attempts = excluded.attempts`,
		d.EventID, d.DeviceID, d.Outcome, nullStr(d.Level), d.At.UTC().Format(timeFormat), nullStr(d.Error),
		nullStr(d.APNsID), d.Attempts)
	return err
}

// deliveryJSON is a Delivery as the read API shows it on an event.
type deliveryJSON struct {
	Device  string `json:"device"`
	Outcome string `json:"outcome"`
	Level   string `json:"level,omitempty"`
	At      string `json:"at"`
	Error   string `json:"error,omitempty"`
}

// WithDeliveries adds each record's push outcomes to its event as delivery,
// in device order. A record without outcomes is left as stored.
func (s *Store) WithDeliveries(ctx context.Context, recs []Record) error {
	if len(recs) == 0 {
		return nil
	}
	args := make([]any, len(recs))
	for i, r := range recs {
		args[i] = r.ID
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_id, device_id, outcome, level, at, error FROM deliveries
		WHERE event_id IN (?`+strings.Repeat(", ?", len(recs)-1)+`) ORDER BY event_id, device_id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	by := map[string][]deliveryJSON{}
	for rows.Next() {
		var ev string
		var d deliveryJSON
		var level, errText sql.NullString
		var at string
		if err := rows.Scan(&ev, &d.Device, &d.Outcome, &level, &at, &errText); err != nil {
			return err
		}
		d.Level, d.Error = level.String, errText.String
		if t, err := time.Parse(timeFormat, at); err == nil {
			d.At = t.Format(time.RFC3339Nano)
		}
		by[ev] = append(by[ev], d)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i, r := range recs {
		ds := by[r.ID]
		if len(ds) == 0 {
			continue
		}
		raw, err := withField(r.Event, "delivery", ds)
		if err != nil {
			return err
		}
		recs[i].Event = raw
	}
	return nil
}

// withField appends "name": v to the JSON object obj, keeping the stored
// field order.
func withField(obj json.RawMessage, name string, v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimRight(string(obj), " \t\r\n")
	if !strings.HasSuffix(trimmed, "}") {
		return nil, errors.New("store: event is not a JSON object")
	}
	body := strings.TrimSpace(trimmed[:len(trimmed)-1])
	sep := ","
	if strings.HasSuffix(body, "{") {
		sep = ""
	}
	return json.RawMessage(body + sep + `"` + name + `":` + string(b) + "}"), nil
}

// pushedWhere is the SQL form of push.Decide's "does this event push a
// visible alert" rule by kind, action, level and hold, for the badge count.
// Passive events never count: digests, incident resolves and passive
// levels, the replacing pushes included. It leaves out expiry, which
// depends on when the push goes out. push's tests check that the two agree.
// An event a mute or an inhibit rule held back has a skipped delivery and
// does not count either.
const pushedWhere = `json_extract(event, '$.hold.store_only') IS NOT 1
	AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.event_id = events.id AND d.outcome = 'skipped')
	AND coalesce(json_extract(event, '$.hold.state'), '') != 'held'
	AND coalesce(level, '') != 'passive'
	AND (kind = 'message'
	  OR (kind = 'incident' AND coalesce(action, '') NOT IN ('ack', 'resolve'))
	  OR (kind = 'checkin' AND (action = 'fail'
	        OR (coalesce(action, '') != 'log' AND coalesce(json_extract(event, '$.checkin.exit_code'), 0) != 0))))`

// Unread counts the events after the read cursor called name that push an
// alert. No cursor counts every such event.
func (s *Store) Unread(ctx context.Context, name string) (int, error) {
	seq, _, err := s.Cursor(ctx, name)
	if err != nil {
		return 0, err
	}
	return s.UnreadAfter(ctx, seq)
}

// UnreadAfter counts the events after seq that push an alert.
func (s *Store) UnreadAfter(ctx context.Context, seq int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE seq > ? AND `+pushedWhere, seq).Scan(&n)
	return n, err
}

// Pushes reports whether the event seq pushes an alert by pushedWhere.
// push's tests use it to check pushedWhere against Decide.
func (s *Store) Pushes(ctx context.Context, seq int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE seq = ? AND `+pushedWhere, seq).Scan(&n)
	return n == 1, err
}

// ReplacesVisible reports whether the event seq of key would replace a
// visible push: an event of key since its last resolve before seq was sent
// to some device at a level other than passive.
func (s *Store) ReplacesVisible(ctx context.Context, key string, seq int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM events e JOIN deliveries d ON d.event_id = e.id
		WHERE e.key = ? AND e.seq < ? AND d.outcome = 'sent' AND coalesce(d.level, '') != 'passive'
		AND e.seq > coalesce((SELECT max(seq) FROM events WHERE key = ? AND seq < ? AND action = 'resolve'), 0)`,
		key, seq, key, seq).Scan(&n)
	return n > 0, err
}

// DeliveryAttempts returns the attempts of each push of the event, in
// device order. Tests use it.
func (s *Store) DeliveryAttempts(ctx context.Context, eventID string) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT attempts FROM deliveries WHERE event_id = ? ORDER BY device_id`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
