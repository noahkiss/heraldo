package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AttachmentDetachedFor is how long an attachment no event names is kept.
const AttachmentDetachedFor = 24 * time.Hour

// ErrUnknownAttachment is returned by Insert and Import for an event that
// names an attachment the store does not hold.
var ErrUnknownAttachment = errors.New("unknown attachment")

// Attachment is one stored file.
type Attachment struct {
	ID          string
	EventID     string // the first event that named it; "" while detached
	ContentType string
	Body        []byte // nil from AttachmentInfos
	Size        int
	SHA256      string
	CreatedAt   time.Time
	CreatedBy   string
}

// AttachmentInfo is an attachment as the read API lists it on an event.
type AttachmentInfo struct {
	ID          string `json:"id"`
	ContentType string `json:"content_type"`
	Bytes       int    `json:"bytes"`
}

// PutAttachment stores a.Body under a.ID. It fills SHA256 and Size, and
// CreatedAt when it is zero. When the same bytes are stored already, it
// stores nothing, fills a with the stored row and reports false; a detached
// row's clock starts again, so the sweep gives it another full day.
func (s *Store) PutAttachment(ctx context.Context, a *Attachment) (bool, error) {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = s.now()
	}
	sum := sha256.Sum256(a.Body)
	a.SHA256, a.Size = hex.EncodeToString(sum[:]), len(a.Body)
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var old Attachment
	var ev sql.NullString
	var at string
	err = tx.QueryRowContext(ctx, `SELECT id, event_id, content_type, bytes, created_at, created_by
		FROM attachments WHERE sha256 = ?`, a.SHA256).Scan(&old.ID, &ev, &old.ContentType, &old.Size, &at, &old.CreatedBy)
	switch {
	case err == nil:
		old.EventID, old.SHA256, old.Body, old.CreatedAt = ev.String, a.SHA256, a.Body, parseTime(at)
		if !ev.Valid {
			old.CreatedAt = a.CreatedAt
			if _, err := tx.ExecContext(ctx, `UPDATE attachments SET created_at = ? WHERE id = ?`,
				fmtTime(a.CreatedAt), old.ID); err != nil {
				return false, err
			}
		}
		*a = old
		return false, tx.Commit()
	case !errors.Is(err, sql.ErrNoRows):
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO attachments
		(id, content_type, bytes, sha256, body, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.ContentType, a.Size, a.SHA256, a.Body, fmtTime(a.CreatedAt), a.CreatedBy); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// GetAttachment returns the attachment with id, with its body.
func (s *Store) GetAttachment(ctx context.Context, id string) (Attachment, error) {
	var a Attachment
	var ev sql.NullString
	var at string
	err := s.db.QueryRowContext(ctx, `SELECT id, event_id, content_type, body, sha256, created_at, created_by
		FROM attachments WHERE id = ?`, id).Scan(&a.ID, &ev, &a.ContentType, &a.Body, &a.SHA256, &at, &a.CreatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	if err != nil {
		return Attachment{}, err
	}
	a.EventID, a.CreatedAt, a.Size = ev.String, parseTime(at), len(a.Body)
	return a, nil
}

// AttachmentInfos returns the attachments with these ids, without bodies, in
// the order of ids. An id the store does not hold is left out.
func (s *Store) AttachmentInfos(ctx context.Context, ids []string) ([]AttachmentInfo, error) {
	by, err := attachmentInfos(ctx, s.db, ids)
	if err != nil {
		return nil, err
	}
	out := make([]AttachmentInfo, 0, len(ids))
	for _, id := range ids {
		if a, ok := by[id]; ok {
			out = append(out, a)
		}
	}
	return out, nil
}

func attachmentInfos(ctx context.Context, q queryer, ids []string) (map[string]AttachmentInfo, error) {
	by := map[string]AttachmentInfo{}
	if len(ids) == 0 {
		return by, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := q.QueryContext(ctx, `SELECT id, content_type, bytes FROM attachments
		WHERE id IN (?`+strings.Repeat(", ?", len(ids)-1)+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a AttachmentInfo
		if err := rows.Scan(&a.ID, &a.ContentType, &a.Bytes); err != nil {
			return nil, err
		}
		by[a.ID] = a
	}
	return by, rows.Err()
}

// linkAttachments checks that the store holds every id an event names, and
// marks each detached one as the event's. It runs in the event's insert.
func linkAttachments(ctx context.Context, tx *sql.Tx, eventID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	by, err := attachmentInfos(ctx, tx, ids)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, ok := by[id]; !ok {
			return fmt.Errorf("%w: %s; upload it with POST /v1/attachments first", ErrUnknownAttachment, id)
		}
	}
	args := []any{eventID}
	for _, id := range ids {
		args = append(args, id)
	}
	_, err = tx.ExecContext(ctx, `UPDATE attachments SET event_id = ?
		WHERE event_id IS NULL AND id IN (?`+strings.Repeat(", ?", len(ids)-1)+`)`, args...)
	return err
}

// SweepAttachments deletes the attachments no event named that were stored
// before cutoff, and returns how many.
func (s *Store) SweepAttachments(ctx context.Context, cutoff time.Time) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.ExecContext(ctx, `DELETE FROM attachments WHERE event_id IS NULL AND created_at < ?`,
		fmtTime(cutoff))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// eventAttachments is the attachments array of a stored event, or nil.
func eventAttachments(raw json.RawMessage) []string {
	if !bytes.Contains(raw, []byte(`"attachments"`)) {
		return nil
	}
	var v struct {
		Attachments []string `json:"attachments"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v.Attachments
}

// WithAttachments adds attachment_info to each record whose event names
// attachments: their id, type and size, in the event's order.
func (s *Store) WithAttachments(ctx context.Context, recs []Record) error {
	names := make([][]string, len(recs))
	var all []string
	seen := map[string]bool{}
	for i, r := range recs {
		names[i] = eventAttachments(r.Event)
		for _, id := range names[i] {
			if !seen[id] {
				seen[id] = true
				all = append(all, id)
			}
		}
	}
	if len(all) == 0 {
		return nil
	}
	by, err := attachmentInfos(ctx, s.db, all)
	if err != nil {
		return err
	}
	for i, r := range recs {
		if len(names[i]) == 0 {
			continue
		}
		info := make([]AttachmentInfo, 0, len(names[i]))
		for _, id := range names[i] {
			if a, ok := by[id]; ok {
				info = append(info, a)
			}
		}
		raw, err := withField(r.Event, "attachment_info", info)
		if err != nil {
			return err
		}
		recs[i].Event = raw
	}
	return nil
}
