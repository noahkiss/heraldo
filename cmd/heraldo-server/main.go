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
//
// Subcommands:
//
//	heraldo-server                    serve (the default)
//	heraldo-server import-tee <dir>   backfill an apprise-tee catalogue
//	heraldo-server replay-tee [-rules <file>] <dir>
//	                                  run a catalogue through the rules, in memory
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
	"strings"
	"syscall"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/config"
	"github.com/noahkiss/heraldo/internal/live"
	"github.com/noahkiss/heraldo/internal/push"
	"github.com/noahkiss/heraldo/internal/replay"
	"github.com/noahkiss/heraldo/internal/rules"
	"github.com/noahkiss/heraldo/internal/server"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/teeimport"
	"github.com/noahkiss/heraldo/internal/validate"
	"github.com/noahkiss/heraldo/internal/version"
)

const usage = `usage: heraldo-server [command]

commands:
  serve              run the server (the default)
  import-tee <dir>   backfill an apprise-tee catalogue directory
  replay-tee [-rules <file>] <dir>
                     run a catalogue through the rules file (default
                     $HERALDO_RULES) in memory and report what they classified
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
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return serve(log)
	case "import-tee":
		if len(args) != 1 {
			return errors.New("usage: heraldo-server import-tee <dir>")
		}
		return importTee(args[0], log)
	case "replay-tee":
		return replayTee(args, stdout)
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

func serve(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, st, err := open(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
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
	pusher, lm, pushState := startPush(ctx, cfg, st, rw, log)
	h := server.NewServer(server.Options{
		Version: version.String(), Store: st, Tokens: tokens, Validator: v, Rules: rw, Logger: log,
		Done: ctx.Done(), Pusher: pusher, PushState: pushState, Live: lm,
	})
	go h.RunCheckins(ctx, checkinTick)
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
			"rules", rw.Rules().Len(), "push", pushState, "version", version.String())
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// checkinTick is how often the scheduler judges the checks.
const checkinTick = 15 * time.Second

// startPush starts the APNs pusher and the Live Activity manager when
// HERALDO_PUSH is on and a key loads. A missing or bad key logs an error and
// leaves push off: ingest keeps working.
func startPush(ctx context.Context, cfg config.Config, st *store.Store, rw *rules.Watcher, log *slog.Logger) (server.Pusher, server.Live, api.HealthPush) {
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
	go p.Run(ctx)
	lm := live.New(live.Config{Store: st, Sender: p, Logger: log})
	go lm.Run(ctx)
	return p, lm, api.HealthPushOn
}

func importTee(dir string, log *slog.Logger) error {
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
	s, err := teeimport.Run(ctx, dir, st, v, rs, log)
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
