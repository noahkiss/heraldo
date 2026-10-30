// Command heraldo-server runs the heraldo notification hub.
//
// Configuration comes from the environment:
//
//	HERALDO_ADDR         listen address, host:port (default :7842)
//	HERALDO_DB           SQLite database path (default heraldo.db)
//	HERALDO_TOKENS_FILE  token list file, one "<name> <scope> <sha256>" per line
//	HERALDO_TOKENS       the same list inline, entries separated by commas
//	HERALDO_RULES        ingest rules file, reloaded when it changes (default: no rules)
//	HERALDO_PUSH         on sends APNs pushes (default off)
//	HERALDO_APNS_KEY_FILE, HERALDO_APNS_KEY_ID, HERALDO_APNS_TEAM_ID
//	                     the APNs auth key (.p8), its key id and the team id
//	HERALDO_APNS_KEY     the .p8 key's PEM text inline, instead of the file
//	HERALDO_NOTIFY_TRUSTED_NETS  CIDRs whose senders may use the Apprise paths
//	                     with no credential (default none)
//	HERALDO_HOLD         the alert hold: off, shadow or on (default off)
//	HERALDO_HOLD_BYPASS_FILE  the hold's bypass list, a JSON file
//	HERALDO_HOLD_DEADLINE, HERALDO_HOLD_DEADMAN  the hold's timers (30m, 20m)
//	HERALDO_ROLE         standby runs this server as a primary's standby (default primary)
//	HERALDO_PRIMARY_URL, HERALDO_PRIMARY_TOKEN
//	                     a standby's primary, and its standby-capable token there
//	HERALDO_STANDBY_INTERVAL, HERALDO_PRIMARY_DOWN_AFTER
//	                     a standby's round interval (1m) and alert delay (10m)
//	HERALDO_SECRET_KEY_FILE  a 32-byte key that encrypts secrets written
//	                     through the settings API (default: such writes are refused)
//
// Server settings (internal/settings, GET /v1/settings) each have an env
// alias and a flag alias: llm.base_url is HERALDO_LLM_BASE_URL and
// --llm-base-url. A flag wins over the env var, the env var over a value
// stored through the API. A secret has no flag.
//
// Subcommands:
//
//	heraldo-server [serve] [--<setting> <value> ...]
//	                                  serve (the default)
//	heraldo-server import-tee [-since YYYY-MM] <dir>
//	                                  backfill an apprise-tee catalogue
//	heraldo-server replay-tee [-rules <file>] <dir>
//	                                  run a catalogue through the rules, in memory
//	heraldo-server backfill-projects [-dry-run] [-rules <file>]
//	                                  file stored events with no project
//	heraldo-server backup <path>      online backup of the database to path
//	heraldo-server hash-token         print the SHA-256 of a token read from stdin
//	heraldo-server healthcheck        exit 0 when the local /healthz answers ok
//	heraldo-server version            print the version
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/config"
	"github.com/noahkiss/heraldo/internal/escalate"
	"github.com/noahkiss/heraldo/internal/hold"
	"github.com/noahkiss/heraldo/internal/live"
	"github.com/noahkiss/heraldo/internal/mcphttp"
	"github.com/noahkiss/heraldo/internal/push"
	"github.com/noahkiss/heraldo/internal/replay"
	"github.com/noahkiss/heraldo/internal/rules"
	"github.com/noahkiss/heraldo/internal/server"
	"github.com/noahkiss/heraldo/internal/settings"
	"github.com/noahkiss/heraldo/internal/standby"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/teeimport"
	"github.com/noahkiss/heraldo/internal/validate"
	"github.com/noahkiss/heraldo/internal/version"
)

const usage = `usage: heraldo-server [command]

commands:
  serve [--<setting> <value> ...]
                     run the server (the default); each server setting has a
                     flag, such as --llm-mode shadow (heraldo-server serve -h
                     lists them)
  import-tee [-since YYYY-MM] <dir>
                     backfill an apprise-tee catalogue directory, from month
                     YYYY-MM on when -since is given
  replay-tee [-rules <file>] <dir>
                     run a catalogue through the rules file (default
                     $HERALDO_RULES) in memory and report what they classified
  backfill-projects [-dry-run] [-rules <file>]
                     give the stored events with no project one, from the
                     rules file's projects map (default $HERALDO_RULES) and
                     from earlier events with the same key; -dry-run only
                     reports
  backup <path>      copy the database to path with SQLite's online backup
  hash-token         print the SHA-256 of a token read from stdin
  healthcheck        exit 0 when this server's /healthz answers ok (for Docker)
  version            print the version
`

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(log)
	if err := run(os.Args[1:], os.Stdin, os.Stdout, log); err != nil {
		log.Error("heraldo-server", "err", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer, log *slog.Logger) error {
	cmd := "serve"
	// heraldo-server --llm-mode shadow is serve with flags.
	if len(args) > 0 && (!strings.HasPrefix(args[0], "-") || isTopLevel(args[0])) {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		flags, err := serveFlags(args, stdout)
		if err != nil || flags == nil {
			return err // nil, nil: -h printed the flags
		}
		return serve(flags, log)
	case "import-tee":
		fs := flag.NewFlagSet("import-tee", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		since := fs.String("since", "", "first month to read, YYYY-MM")
		if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
			return errors.New("usage: heraldo-server import-tee [-since YYYY-MM] <dir>")
		}
		return importTee(fs.Arg(0), *since, log)
	case "replay-tee":
		return replayTee(args, stdout)
	case "backfill-projects":
		return backfillProjects(args, stdout)
	case "backup":
		if len(args) != 1 {
			return errors.New("usage: heraldo-server backup <path>")
		}
		return backup(args[0], log)
	case "hash-token":
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		tok := strings.TrimSpace(line)
		if tok == "" {
			return errors.New("hash-token: no token on stdin")
		}
		fmt.Fprintln(stdout, auth.Hash(tok))
		return nil
	case "healthcheck":
		return healthcheck()
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "heraldo-server", version.String())
		return nil
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
}

// isTopLevel reports whether a is a top-level flag, not one of serve's.
func isTopLevel(a string) bool { return a == "--help" || a == "-h" || a == "--version" || a == "-v" }

// serveFlags parses serve's flags: one per server setting. A flag that
// would carry a secret is refused before parsing, and its value is never
// echoed. nil, nil means -h printed the flags.
func serveFlags(args []string, stdout io.Writer) (map[string]string, error) {
	if err := settings.RefuseSecretFlags(args); err != nil {
		return nil, err
	}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stdout)
	got := settings.Flags(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, nil
		}
		return nil, fmt.Errorf("serve: %w", err)
	}
	if fs.NArg() != 0 {
		return nil, fmt.Errorf("serve: unexpected argument %q", fs.Arg(0))
	}
	return got(), nil
}

// loadSettings builds the settings store from the flags, the environment
// and the settings table.
func loadSettings(ctx context.Context, cfg config.Config, st *store.Store, flags map[string]string, log *slog.Logger) (*settings.Manager, error) {
	var key []byte
	if cfg.SecretKeyFile != "" {
		var err error
		if key, err = settings.LoadSecretKey(cfg.SecretKeyFile); err != nil {
			return nil, err
		}
	}
	m, err := settings.New(ctx, settings.Options{Backend: st, Getenv: os.Getenv, Flags: flags, SecretKey: key, Logger: log})
	if err != nil {
		return nil, err
	}
	var locked []string
	for _, v := range m.All() {
		if v.LockedBy != "" {
			locked = append(locked, v.Key.Name)
		}
	}
	log.Info("settings", "locked", strings.Join(locked, " "), "secret_key", key != nil)
	return m, nil
}

func open(ctx context.Context) (config.Config, *store.Store, error) {
	cfg, err := config.FromEnv(os.Getenv)
	if err != nil {
		return cfg, nil, err
	}
	st, err := store.Open(ctx, cfg.DB)
	if err != nil {
		return cfg, nil, fmt.Errorf("open %s: %w", cfg.DB, err)
	}
	return cfg, st, nil
}

func serve(flags map[string]string, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, st, err := open(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	sm, err := loadSettings(ctx, cfg, st, flags, log)
	if err != nil {
		return err
	}
	settings.SetDefault(sm)
	st.SetFlapPolicy(flapPolicy)
	tokens, err := cfg.LoadTokens()
	if err != nil {
		return err
	}
	if tokens.Len() == 0 {
		log.Warn("no tokens configured; every call except /healthz is refused")
	}
	v, err := validate.New()
	if err != nil {
		return err
	}
	var rw *rules.Watcher
	if cfg.RulesFile != "" {
		rw, err = rules.NewWatcher(cfg.RulesFile, log)
		if err != nil {
			return fmt.Errorf("HERALDO_RULES: %w", err)
		}
		go rw.Watch(ctx, rules.DefaultInterval)
	}
	bypass, err := hold.NewBypass(cfg.HoldBypassFile, log)
	if err != nil {
		return fmt.Errorf("HERALDO_HOLD_BYPASS_FILE: %w", err)
	}
	// The pusher outlives ctx: at shutdown it drains its queue first.
	pushCtx, stopPush := context.WithCancel(context.Background())
	defer stopPush()
	pusher, lm, pushState := startPush(ctx, pushCtx, cfg, st, rw, log)
	// Escalation does not depend on push: it runs with push off too.
	esc := escalate.New(escalate.Config{Store: st, Rules: rw.Rules, Logger: log.With("component", "escalate")})
	go esc.Run(pushCtx)
	// Critical events still queued when the server last stopped.
	if n, err := esc.Sweep(ctx, time.Now().Add(-escalate.SweepWindow)); err != nil {
		log.Error("escalate: start-up sweep", "err", err)
	} else if n > 0 {
		log.Info("escalate: queued critical events left unchecked", "events", n)
	}
	so := server.Options{
		Version: version.String(), Store: st, Tokens: tokens, Validator: v, Rules: rw, Logger: log,
		Done: ctx.Done(), Pusher: pusher, Escalator: esc, PushState: pushState, Live: lm,
		TrustedNets: cfg.NotifyTrustedNets, Settings: sm,
		Standby: cfg.Standby,
		// The chat model reads through the MCP tools in process, under a
		// read grant.
		AskTools: mcphttp.NewToolbox(version.String()),
		Hold: server.HoldOptions{
			Mode: cfg.HoldMode, Bypass: bypass, Deadline: cfg.HoldDeadline, Deadman: cfg.HoldDeadman,
		},
	}
	if cfg.MCPHTTP {
		so.MCP = mcphttp.New(mcphttp.Options{Version: version.String(), Logger: log.With("component", "mcp")})
		log.Info("mcp: serving the MCP tools at POST /v1/mcp")
	}
	h := server.NewServer(so)
	if len(cfg.NotifyTrustedNets) > 0 {
		log.Warn("notify: senders in these networks post to the Apprise paths with no credential",
			"nets", fmt.Sprint(cfg.NotifyTrustedNets))
	}
	log.Info("hold", "mode", cfg.HoldMode, "deadline", cfg.HoldDeadline.String(), "deadman", cfg.HoldDeadman.String())
	log.Info("llm", "mode", h.LLM().Mode(), "health", string(h.LLM().Health()), "host", h.LLM().BaseHost())
	go h.RunHold(ctx, holdTick)
	go h.RunAttachmentSweep(ctx, server.AttachmentSweepEvery)
	// Both a primary and a standby: each serves /v1/revisions from its own
	// state.
	go h.RunTimeChanges(ctx, timeChangeTick)
	if cfg.Standby {
		// A standby judges no checks and sends no digest: the primary owns
		// them, and a standby that ran them would raise a second copy of
		// every incident and digest.
		if pushState != api.HealthPushOn {
			log.Warn("standby: push is not on; this standby delivers nothing")
		}
		go standby.New(standby.Config{
			PrimaryURL: cfg.PrimaryURL, Token: cfg.PrimaryToken, Store: st, Emit: h.Emit,
			Interval: cfg.StandbyInterval, DownAfter: cfg.PrimaryDownAfter, Logger: log,
		}).Run(ctx)
	} else {
		go h.RunCheckins(ctx, checkinTick)
		go h.RunDigest(ctx, server.DigestTick)
	}
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "db", cfg.DB, "tokens", tokens.Len(),
			"rules", rw.Rules().Len(), "push", pushState, "standby", cfg.Standby, "version", version.String())
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	// One budget for both steps, inside a container runtime's usual 10 s
	// stop timeout.
	shutdown, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		return err
	}
	if p, ok := pusher.(*push.Pusher); ok && !p.Drain(shutdown) {
		log.Warn("push: queue not drained at shutdown; the next start sweeps it")
	}
	if !esc.Drain(shutdown) {
		log.Warn("escalate: queue not drained at shutdown")
	}
	stopPush()
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// sweepWindow is how far back the start-up sweep looks for events that
// have no delivery.
const sweepWindow = time.Hour

// checkinTick is how often the scheduler judges the checks.
const checkinTick = 15 * time.Second

// holdTick is how often the hold checks its deadlines and the dead-man.
const holdTick = 30 * time.Second

// timeChangeTick is how often the server looks for read answers that time
// alone changed (a flap settling, an incident turning stale, a mute running
// out), to bump their revisions.
const timeChangeTick = 30 * time.Second

// startPush starts the APNs pusher and the Live Activity manager when
// HERALDO_PUSH is on and a key loads. A missing or bad key logs an error and
// leaves push off: ingest keeps working.
//
// The pusher runs on pushCtx, which the caller ends after draining its
// queue; the Live Activity manager runs on ctx. Before it returns, it queues
// the events of the last sweepWindow that have no delivery yet.
func startPush(ctx, pushCtx context.Context, cfg config.Config, st *store.Store, rw *rules.Watcher, log *slog.Logger) (server.Pusher, server.Live, api.HealthPush) {
	if !cfg.Push {
		if cfg.HasAPNsKey() {
			log.Info("push: an APNs key is set, but HERALDO_PUSH is off; nothing is pushed")
		}
		return nil, nil, api.HealthPushOff
	}
	if !cfg.HasAPNsKey() {
		log.Error("push: HERALDO_PUSH is on, but no APNs key is set; nothing is pushed")
		return nil, nil, api.HealthPushNoKey
	}
	pem, err := cfg.LoadAPNsKey()
	if err != nil {
		log.Error("push: the APNs key does not load; nothing is pushed", "err", err)
		return nil, nil, api.HealthPushNoKey
	}
	key, err := push.ParseKey(pem)
	if err != nil {
		log.Error("push: the APNs key does not parse; nothing is pushed", "err", err)
		return nil, nil, api.HealthPushNoKey
	}
	p := push.New(push.Config{Key: key, KeyID: cfg.APNsKeyID, TeamID: cfg.APNsTeamID, Store: st, Logger: log, Rules: rw.Rules})
	go p.Run(pushCtx)
	// The badge counts from the inbox cursor. Without one, it starts here:
	// the history from before push went on is not unread.
	if made, err := st.StartCursor(ctx, push.BadgeCursor, "heraldo-server"); err != nil {
		log.Error("push: start the badge cursor", "err", err)
	} else if made {
		log.Info("push: started the badge cursor at the newest event", "cursor", push.BadgeCursor)
	}
	if n, err := p.Sweep(ctx, time.Now().Add(-sweepWindow)); err != nil {
		log.Error("push: start-up sweep", "err", err)
	} else if n > 0 {
		log.Info("push: queued events left unpushed", "events", n)
	}
	lc := live.Config{Store: st, Sender: p, Logger: log}
	if cfg.LiveChannels {
		lc.Channels = p
	} else {
		log.Info("live: HERALDO_LIVE_CHANNELS is off; Live Activities update by token only")
	}
	lm := live.New(lc)
	go lm.Run(ctx)
	return p, lm, api.HealthPushOn
}

func importTee(dir, since string, log *slog.Logger) error {
	ctx := context.Background()
	cfg, st, err := open(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	v, err := validate.New()
	if err != nil {
		return err
	}
	var rs *rules.Set
	if cfg.RulesFile != "" {
		if rs, err = rules.Load(cfg.RulesFile); err != nil {
			return fmt.Errorf("HERALDO_RULES: %w", err)
		}
	}
	s, err := teeimport.RunSince(ctx, dir, since, st, v, rs, log)
	log.Info("tee import", "files", s.Files, "lines", s.Lines, "imported", s.Imported,
		"already_imported", s.Skipped, "bad", s.Bad, "legacy_ids", s.LegacyIDs, "typed", s.Typed)
	return err
}

// replayTee runs a catalogue through a rules file without a database.
func replayTee(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("replay-tee", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("rules", os.Getenv("HERALDO_RULES"), "rules file")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return errors.New("usage: heraldo-server replay-tee [-rules <file>] <dir>")
	}
	var rs *rules.Set
	if *path != "" {
		var err error
		if rs, err = rules.Load(*path); err != nil {
			return err
		}
	}
	v, err := validate.New()
	if err != nil {
		return err
	}
	r, err := replay.Run(fs.Arg(0), rs, v)
	if err != nil {
		return err
	}
	r.Write(stdout)
	return nil
}

// backfillProjects files the stored events that have no project.
func backfillProjects(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("backfill-projects", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dry := fs.Bool("dry-run", false, "report, change nothing")
	path := fs.String("rules", os.Getenv("HERALDO_RULES"), "rules file")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New("usage: heraldo-server backfill-projects [-dry-run] [-rules <file>]")
	}
	var rs *rules.Set
	if *path != "" {
		var err error
		if rs, err = rules.Load(*path); err != nil {
			return err
		}
	}
	ctx := context.Background()
	_, st, err := open(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	res, err := st.BackfillProjects(ctx, func(f map[string]any) string {
		p, _ := rs.MapProject(f)
		return p
	}, *dry)
	if err != nil {
		return fmt.Errorf("backfill-projects: %w", err)
	}
	verb := "filed"
	if *dry {
		verb = "would file"
	}
	fmt.Fprintf(stdout, "events with no project: %d; %s %d, %d left without one\n",
		res.Scanned, verb, res.Total(), res.Scanned-res.Total())
	names := make([]string, 0, len(res.Changed))
	for p := range res.Changed {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		fmt.Fprintf(stdout, "  %5d  %s\n", res.Changed[p], p)
	}
	fmt.Fprintf(stdout, "state rows %s: %d\n", verb, res.Derived)
	return nil
}

func backup(dst string, log *slog.Logger) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("backup: %s exists; give a new path", dst)
	}
	cfg, err := config.FromEnv(os.Getenv)
	if err != nil {
		return err
	}
	if _, err := os.Stat(cfg.DB); err != nil {
		return fmt.Errorf("backup: database: %w", err)
	}
	ctx := context.Background()
	_, st, err := open(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	start := time.Now()
	if err := st.Backup(ctx, dst); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	log.Info("backup", "path", dst, "ms", time.Since(start).Milliseconds())
	return nil
}

// healthcheck asks the server on HERALDO_ADDR for /healthz. The image has no
// shell or curl, so Docker's HEALTHCHECK runs this.
func healthcheck() error {
	cfg, err := config.FromEnv(os.Getenv)
	if err != nil {
		return err
	}
	host, port, _ := net.SplitHostPort(cfg.Addr)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	return nil
}

// flapPolicy reads the flap.* settings for the store's flap grouping.
func flapPolicy() store.FlapPolicy {
	return store.FlapPolicy{
		Enabled: settings.Get[bool]("flap.enabled"),
		Opens:   settings.Get[int]("flap.opens"),
		Window:  settings.Get[time.Duration]("flap.window"),
		Settle:  settings.Get[time.Duration]("flap.settle"),
	}
}
