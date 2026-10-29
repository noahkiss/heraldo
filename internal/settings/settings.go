package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/heraldo/internal/store"
)

// Source says where a key's value comes from.
type Source string

// Sources, highest precedence first.
const (
	FromFlag    Source = "flag"
	FromEnv     Source = "env"
	FromDB      Source = "db"
	FromDefault Source = "default"
)

// Backend is the settings table. *store.Store is one.
type Backend interface {
	Settings(ctx context.Context) ([]store.Setting, error)
	PutSetting(ctx context.Context, st store.Setting) error
	DeleteSetting(ctx context.Context, key string) (bool, error)
}

var _ Backend = (*store.Store)(nil)

// Options configure a Manager.
type Options struct {
	// Backend is the settings table; nil keeps writes in memory only.
	Backend Backend
	// Getenv reads the env aliases; nil reads none.
	Getenv func(string) string
	// Flags are the launch flags given, by key name, as text.
	Flags map[string]string
	// SecretKey encrypts secrets at rest; nil refuses secret writes.
	SecretKey []byte
	// Now is the clock; nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

type pinned struct {
	v any
	// by is the flag (--llm-base-url) or variable that set it.
	by string
}

type stored struct {
	v   any
	raw json.RawMessage
	at  time.Time
	by  string
	err error // a secret that does not decrypt, or a value that no longer parses
}

// Manager holds every key's value and tells subscribers when one changes.
// It is safe for concurrent use.
type Manager struct {
	o     Options
	flags map[string]pinned
	env   map[string]pinned

	mu   sync.RWMutex
	db   map[string]stored
	subs map[*sub]struct{}
}

type sub struct {
	prefixes []string
	ch       chan string
}

// New reads the flags and env aliases, then loads the settings table. A bad
// flag or env value is an error: the server must not start on it. A stored
// row that no longer parses is logged and ignored.
func New(ctx context.Context, o Options) (*Manager, error) {
	if o.Getenv == nil {
		o.Getenv = func(string) string { return "" }
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	m := &Manager{o: o, flags: map[string]pinned{}, env: map[string]pinned{}, db: map[string]stored{},
		subs: map[*sub]struct{}{}}
	for name, text := range o.Flags {
		k, ok := Lookup(name)
		if !ok {
			return nil, fmt.Errorf("--%s: %w", flagName(name), ErrUnknown)
		}
		if k.IsSecret() {
			return nil, secretFlagError(k)
		}
		v, err := k.ParseText(text)
		if err != nil {
			return nil, fmt.Errorf("--%s: %v", k.Flag(), err)
		}
		m.flags[name] = pinned{v: v, by: "--" + k.Flag()}
	}
	for _, k := range registry {
		if err := m.readEnv(k); err != nil {
			return nil, err
		}
	}
	if o.Backend != nil {
		rows, err := o.Backend.Settings(ctx)
		if err != nil {
			return nil, fmt.Errorf("settings: %w", err)
		}
		for _, r := range rows {
			k, ok := Lookup(r.Key)
			if !ok {
				o.Logger.Warn("settings: a stored key is not known; ignored", "key", r.Key)
				continue
			}
			s := stored{raw: r.Value, at: r.UpdatedAt, by: r.UpdatedBy}
			if k.IsSecret() {
				s.v, s.err = unseal(o.SecretKey, k.Name, r.Value)
			} else {
				s.v, s.err = k.ParseJSON(r.Value)
			}
			if s.err != nil {
				// Never the value: it may be a secret.
				o.Logger.Warn("settings: a stored value is not usable; the key falls back", "key", k.Name, "err", s.err)
			}
			m.db[k.Name] = s
		}
	}
	return m, nil
}

func (m *Manager) readEnv(k Key) error {
	name, text := k.Env(), m.o.Getenv(k.Env())
	if k.IsSecret() {
		file := m.o.Getenv(k.EnvFile())
		if text != "" && file != "" {
			return fmt.Errorf("set %s or %s, not both", k.Env(), k.EnvFile())
		}
		if file != "" {
			b, err := os.ReadFile(file)
			if err != nil {
				return fmt.Errorf("%s: %w", k.EnvFile(), err)
			}
			name, text = k.EnvFile(), strings.TrimRight(string(b), "\r\n")
			if text == "" {
				return fmt.Errorf("%s: the file is empty", k.EnvFile())
			}
		}
	}
	if text == "" {
		return nil
	}
	v, err := k.ParseText(text)
	if err != nil {
		if k.IsSecret() {
			return fmt.Errorf("%s: %v", name, err)
		}
		return fmt.Errorf("%s %q: %v", name, text, err)
	}
	m.env[k.Name] = pinned{v: v, by: name}
	return nil
}

func secretFlagError(k Key) error {
	return fmt.Errorf("--%s: a flag never carries a secret; set %s or %s", flagName(k.Name), k.EnvFile(), k.Env())
}

// View is one key's value as the API reports it. A secret's Value is nil.
type View struct {
	Key    Key
	Value  any
	Source Source
	// Set reports, for a secret, whether it has a value.
	Set bool
	// LockedBy names the flag or env var that sets the key; a write is
	// refused while it does.
	LockedBy  string
	UpdatedAt time.Time
	UpdatedBy string
	// Error says why a stored value is not used.
	Error string
}

// resolve returns the value in force for k and where it comes from. The
// caller holds mu.
func (m *Manager) resolve(k Key) (any, Source, string) {
	if p, ok := m.flags[k.Name]; ok {
		return p.v, FromFlag, p.by
	}
	if p, ok := m.env[k.Name]; ok {
		return p.v, FromEnv, p.by
	}
	if s, ok := m.db[k.Name]; ok && s.err == nil {
		return s.v, FromDB, ""
	}
	return k.Default, FromDefault, ""
}

func (m *Manager) view(k Key) View {
	v, src, by := m.resolve(k)
	out := View{Key: k, Value: k.JSONValue(v), Source: src, LockedBy: by}
	if s, ok := m.db[k.Name]; ok {
		out.UpdatedAt, out.UpdatedBy = s.at, s.by
		if s.err != nil {
			out.Error = s.err.Error()
		}
	}
	if k.IsSecret() {
		out.Value = nil
		out.Set = v.(string) != ""
		if s, ok := m.db[k.Name]; ok && src == FromDefault && s.err != nil {
			// Stored, but not usable: report it as set so the operator
			// sees the error rather than an empty key.
			out.Set, out.Source = true, FromDB
		}
	}
	return out
}

// View returns one key's view.
func (m *Manager) View(name string) (View, error) {
	k, ok := Lookup(name)
	if !ok {
		return View{}, ErrUnknown
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.view(k), nil
}

// All returns every key's view, sorted by name.
func (m *Manager) All() []View {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []View
	for _, k := range Keys() {
		out = append(out, m.view(k))
	}
	return out
}

// Value returns the value in force for name, in the key's Go type: a
// secret's plain text too. Never log or return a secret's value.
func (m *Manager) Value(name string) (any, error) {
	k, ok := Lookup(name)
	if !ok {
		return nil, ErrUnknown
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, _, _ := m.resolve(k)
	return v, nil
}

// LockedError is a write to a key a flag or env var sets.
type LockedError struct{ Key, By string }

func (e *LockedError) Error() string {
	return fmt.Sprintf("%s is set by %s; change or remove it there", e.Key, e.By)
}

// ErrNoSecretKey is a secret write on a server without a secret key file.
var ErrNoSecretKey = errors.New("this server stores no secrets: set " + SecretKeyEnv +
	" to a file with a 32-byte key (openssl rand -hex 32), or set the secret's env var")

// InvalidError is a value the key's type refuses.
type InvalidError struct {
	Key string
	Err error
}

func (e *InvalidError) Error() string { return e.Key + ": " + e.Err.Error() }
func (e *InvalidError) Unwrap() error { return e.Err }

func (m *Manager) lockedBy(k Key) string {
	if p, ok := m.flags[k.Name]; ok {
		return p.by
	}
	if p, ok := m.env[k.Name]; ok {
		return p.by
	}
	return ""
}

// Set stores raw, a JSON value, for name. by names the token that wrote it.
func (m *Manager) Set(ctx context.Context, name string, raw json.RawMessage, by string) (View, error) {
	k, ok := Lookup(name)
	if !ok {
		return View{}, ErrUnknown
	}
	if b := m.lockedBy(k); b != "" {
		return View{}, &LockedError{Key: k.Name, By: b}
	}
	v, err := k.ParseJSON(raw)
	if err != nil {
		return View{}, &InvalidError{Key: k.Name, Err: err}
	}
	row := store.Setting{Key: k.Name, UpdatedAt: m.o.Now().UTC(), UpdatedBy: by}
	if k.IsSecret() {
		if v.(string) == "" {
			return View{}, &InvalidError{Key: k.Name, Err: errors.New("an empty secret; unset it instead")}
		}
		if m.o.SecretKey == nil {
			return View{}, ErrNoSecretKey
		}
		if row.Value, err = seal(m.o.SecretKey, k.Name, v.(string)); err != nil {
			return View{}, err
		}
	} else if row.Value, err = json.Marshal(k.JSONValue(v)); err != nil {
		return View{}, err
	}
	m.mu.Lock()
	if m.o.Backend != nil {
		if err := m.o.Backend.PutSetting(ctx, row); err != nil {
			m.mu.Unlock()
			return View{}, err
		}
	}
	m.db[k.Name] = stored{v: v, raw: row.Value, at: row.UpdatedAt, by: by}
	out := m.view(k)
	m.mu.Unlock()
	m.notify(k.Name)
	return out, nil
}

// Unset removes name's stored value, so it falls back to its default.
func (m *Manager) Unset(ctx context.Context, name, by string) (View, error) {
	k, ok := Lookup(name)
	if !ok {
		return View{}, ErrUnknown
	}
	if b := m.lockedBy(k); b != "" {
		return View{}, &LockedError{Key: k.Name, By: b}
	}
	m.mu.Lock()
	if m.o.Backend != nil {
		if _, err := m.o.Backend.DeleteSetting(ctx, k.Name); err != nil {
			m.mu.Unlock()
			return View{}, err
		}
	}
	_, had := m.db[k.Name]
	delete(m.db, k.Name)
	out := m.view(k)
	m.mu.Unlock()
	if had {
		m.notify(k.Name)
	}
	return out, nil
}

// Subscribe returns a channel that gets a key's name after a write changes
// it. With prefixes, only keys that start with one, such as "quiet.". A slow
// reader may miss names while its buffer is full, so on any name a reader
// re-reads every key it uses. cancel ends the subscription.
func (m *Manager) Subscribe(prefixes ...string) (<-chan string, func()) {
	s := &sub{prefixes: prefixes, ch: make(chan string, 8)}
	m.mu.Lock()
	m.subs[s] = struct{}{}
	m.mu.Unlock()
	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			m.mu.Lock()
			delete(m.subs, s)
			m.mu.Unlock()
		})
	}
}

func (m *Manager) notify(name string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for s := range m.subs {
		if !matches(s.prefixes, name) {
			continue
		}
		select {
		case s.ch <- name:
		default:
		}
	}
}

func matches(prefixes []string, name string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// From returns the value in force for name as T: bool, int, string,
// time.Duration or []string, by the key's Type. An unknown key or a wrong T
// panics: both are programming errors.
func From[T any](m *Manager, name string) T {
	v, err := m.Value(name)
	if err != nil {
		panic("settings: " + name + ": " + err.Error())
	}
	t, ok := v.(T)
	if !ok {
		var zero T
		panic(fmt.Sprintf("settings: %s is %T, not %T", name, v, zero))
	}
	return t
}

var (
	dmu sync.RWMutex
	def *Manager
)

// SetDefault makes m the Manager Get reads. heraldo-server calls it once at
// start.
func SetDefault(m *Manager) {
	dmu.Lock()
	def = m
	dmu.Unlock()
}

// Default returns the Manager Get reads. Before SetDefault, it is one with
// only the built-in defaults.
func Default() *Manager {
	dmu.RLock()
	m := def
	dmu.RUnlock()
	if m != nil {
		return m
	}
	dmu.Lock()
	defer dmu.Unlock()
	if def == nil {
		def, _ = New(context.Background(), Options{})
	}
	return def
}

// Get returns the value in force for name from the default Manager. See
// From.
func Get[T any](name string) T { return From[T](Default(), name) }
