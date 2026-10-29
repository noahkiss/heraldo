package store

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestAttachmentDedupAndSweep(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	t0 := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	a := Attachment{ID: "att_01JBA6Z0000000000000000001", ContentType: "text/plain", Body: []byte("x"), CreatedAt: t0, CreatedBy: "t"}
	if created, err := s.PutAttachment(ctx, &a); err != nil || !created {
		t.Fatalf("put: %v %v", created, err)
	}
	// The same bytes 20 hours later: the first row, and its clock starts again.
	b := Attachment{ID: "att_01JBA6Z0000000000000000002", ContentType: "text/plain", Body: []byte("x"), CreatedAt: t0.Add(20 * time.Hour)}
	if created, err := s.PutAttachment(ctx, &b); err != nil || created || b.ID != a.ID {
		t.Fatalf("dedup: %v %v %s", created, err, b.ID)
	}
	if n, err := s.SweepAttachments(ctx, t0.Add(25*time.Hour).Add(-AttachmentDetachedFor)); err != nil || n != 0 {
		t.Fatalf("sweep after a re-upload: %d %v", n, err)
	}
	if n, err := s.SweepAttachments(ctx, t0.Add(45*time.Hour).Add(-AttachmentDetachedFor)); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	if _, err := s.GetAttachment(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after sweep: %v", err)
	}
}

// heraldo-server backup copies the database file, so attachments ride along.
func TestBackupKeepsAttachments(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, filepath.Join(dir, "h.db"))
	defer s.Close()
	body := bytes.Repeat([]byte{0xab}, 300<<10)
	a := Attachment{ID: "att_01JBA6Z0000000000000000003", ContentType: "image/png", Body: body, CreatedBy: "t"}
	if _, err := s.PutAttachment(ctx, &a); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "backup.db")
	if err := s.Backup(ctx, dst); err != nil {
		t.Fatal(err)
	}
	c := open(t, dst)
	defer c.Close()
	got, err := c.GetAttachment(ctx, a.ID)
	if err != nil || !bytes.Equal(got.Body, body) || got.SHA256 != a.SHA256 {
		t.Fatalf("backup: %v, %d bytes", err, len(got.Body))
	}
}
