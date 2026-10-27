package rules

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// DefaultInterval is how often a Watcher checks the rules file.
const DefaultInterval = 5 * time.Second

// Watcher holds the rules from one file and reloads them when the file
// changes. A file that fails to load keeps the rules already in force and
// logs an error. It is safe for concurrent use.
type Watcher struct {
	path string
	log  *slog.Logger
	cur  atomic.Pointer[Set]
	raw  []byte
	// failed is the last read error, logged once rather than every tick.
	failed string
}

// NewWatcher loads path. It fails when the first load fails, so a server
// never starts on a broken rules file.
func NewWatcher(path string, log *slog.Logger) (*Watcher, error) {
	if log == nil {
		log = slog.Default()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	w := &Watcher{path: path, log: log, raw: raw}
	w.cur.Store(s)
	return w, nil
}

// Rules returns the rules in force. A nil Watcher has none.
func (w *Watcher) Rules() *Set {
	if w == nil {
		return nil
	}
	return w.cur.Load()
}

// Reload reads the file again and swaps in its rules when its content
// changed. It reports whether the rules changed. Watch calls it from one
// goroutine; callers must not run two at once.
func (w *Watcher) Reload() bool {
	raw, err := os.ReadFile(w.path)
	if err != nil {
		if err.Error() != w.failed {
			w.failed = err.Error()
			w.log.Error("rules: reload failed; keeping the current rules", "path", w.path, "err", err)
		}
		return false
	}
	w.failed = ""
	if bytes.Equal(raw, w.raw) {
		return false
	}
	s, err := Parse(raw)
	w.raw = raw
	if err != nil {
		w.log.Error("rules: reload failed; keeping the current rules", "path", w.path, "err", err)
		return false
	}
	w.cur.Store(s)
	w.log.Info("rules: reloaded", "path", w.path, "rules", s.Len())
	return true
}

// Watch calls Reload every interval until ctx ends. It reads the file
// rather than waiting on file events, so it follows an editor's rename and a
// bind mount alike.
func (w *Watcher) Watch(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Reload()
		}
	}
}
