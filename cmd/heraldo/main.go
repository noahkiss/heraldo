// Command heraldo is the heraldo CLI: an inbox over a heraldo server, and a
// drop-in for a notify script that posts through an Apprise-compatible URL.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/noahkiss/heraldo/internal/client"
	"github.com/noahkiss/heraldo/internal/version"
)

const usage = `usage: heraldo <command> [flags]

reading (HERALDO_URL, HERALDO_TOKEN):
  peek             events since this client's read cursor; --mark moves it
  show <id>        one event with its full body and its attachments
  attachment get [-o file] <id>
                   an attachment's bytes, to a file or stdout
  why <id>         why an event did or did not push, stage by stage
  ls               the newest events, without routine ones; --kind --source --host
                   --key --tag --project -q --open --stale --routine --view
  views [ls]       the saved views every client shares
  held             events held for triage
  hold             the alert hold's queue (a look, not a triage poll; --poll for one)
  state            open incidents, running jobs, checks and statuses; --project
  digest           the daily digest as it would go out now; --send sends it
                   (HERALDO_WRITE_TOKEN)
  projects         the projects stored events name, with counts
  activities [--all]
                   running Live Activities; activities show <key> for one
  devices          registered devices; devices retire <id> stops pushes to one
  checkins         every check: state, next due, last run, last log
  checkins show <slug>
                   one check with its full last log
  tail             follow new events as they arrive; --exec runs a command per event

writing:
  ack <key>        acknowledge an open incident (HERALDO_WRITE_TOKEN)
  resolve <key>    resolve an open incident (HERALDO_WRITE_TOKEN)
  checkins create|pause|resume <slug>
                   add or change a check, pause or resume it (HERALDO_WRITE_TOKEN)
  checkins key [--remove] <slug>
                   print a new ping URL for a check; the old one stops working
  mute             mute a source, key or project for a while (HERALDO_WRITE_TOKEN)
  mutes [end <id>] the mutes in force, or end one (HERALDO_WRITE_TOKEN)
  views save [flags] <name>
                   save a view, or replace the one with that name; --bundle 15m
                   folds a burst of its pushes into one notification (HERALDO_WRITE_TOKEN)
  views rm <name>  remove a saved view (HERALDO_WRITE_TOKEN)
  snooze <key|id>  mute one incident for a while (--for 1h), then push it again if
                   it is still open (HERALDO_WRITE_TOKEN)
  hold release|suppress <id>...
                   close held entries with one diagnosis, or with none
  send             send a notification through HERALDO_SEND_URL or APPRISE_URL
                   --attach <file> (repeatable) posts to the heraldo API instead

server settings (reads: HERALDO_TOKEN; writes: HERALDO_ADMIN_TOKEN):
  settings [ls]    every server setting, its value and where it comes from
  settings get <key>
  settings set <key> <value>
                   store a value; a secret key takes - and reads stdin
  settings unset <key>
                   back to the default

  mcp [--read-only]
                   serve the MCP tools for coding agents on stdio (docs/mcp.md)

  version          print the version

Settings come from the environment, then from $XDG_CONFIG_HOME/heraldo/env
(or HERALDO_ENV_FILE). Run heraldo <command> -h for a command's flags.
`

// app is one run of the CLI, with its outside world made replaceable for tests.
type app struct {
	stdout, stderr io.Writer
	stdin          io.Reader
	// stdinIsTTY reports whether stdin is a terminal, as notify's [ -t 0 ].
	stdinIsTTY func() bool
	getenv     func(string) string
	lookPath   func(string) (string, error)
	http       *http.Client
	// file is the env file's settings.
	file map[string]string
	// pollEvery is tail's poll interval when the server has no stream.
	pollEvery time.Duration
	// retryMax caps tail's reconnect backoff.
	retryMax time.Duration
}

func main() {
	a := &app{
		stdout: os.Stdout, stderr: os.Stderr, stdin: os.Stdin,
		stdinIsTTY: func() bool {
			fi, err := os.Stdin.Stat()
			return err == nil && fi.Mode()&os.ModeCharDevice != 0
		},
		getenv: os.Getenv, lookPath: exec.LookPath,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := a.run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

// run is kept for the stage 0 tests.
func run(args []string, stdout, stderr io.Writer) int {
	a := &app{stdout: stdout, stderr: stderr, stdin: strings.NewReader(""), getenv: os.Getenv,
		lookPath: exec.LookPath, stdinIsTTY: func() bool { return true }}
	return a.run(context.Background(), args)
}

// errUsage marks an error the command already reported with its usage.
var errUsage = errors.New("usage")

func (a *app) run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.stderr, usage)
		return 2
	}
	if a.pollEvery == 0 {
		a.pollEvery = time.Second
	}
	if a.retryMax == 0 {
		a.retryMax = 30 * time.Second
	}
	cmds := map[string]func(context.Context, []string) error{
		"peek": a.peek, "show": a.show, "ls": a.ls, "held": a.held, "checkins": a.checkins,
		"tail": a.tail, "ack": a.ack, "resolve": a.resolve, "send": a.send, "attachment": a.attachment,
		"mcp": a.mcp, "settings": a.settings, "why": a.why,
		"mute": a.mute, "mutes": a.mutes, "snooze": a.snooze, "hold": a.hold, "devices": a.devices,
		"projects": a.projects, "state": a.state, "activities": a.activities, "views": a.views,
		"digest": a.digest,
	}
	name := args[0]
	switch name {
	case "version", "--version", "-v":
		fmt.Fprintln(a.stdout, "heraldo", version.String())
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(a.stdout, usage)
		return 0
	}
	cmd, ok := cmds[name]
	if !ok {
		fmt.Fprintf(a.stderr, "heraldo: unknown command %q\n\n%s", name, usage)
		return 2
	}
	if err := a.loadEnvFile(); err != nil {
		fmt.Fprintf(a.stderr, "heraldo: env file: %v\n", err)
		return 1
	}
	err := cmd(ctx, args[1:])
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		return 2
	case errors.Is(err, errQuiet):
		return 1
	default:
		fmt.Fprintf(a.stderr, "heraldo %s: %v\n", name, err)
		return 1
	}
}

// errQuiet is a failure the command already reported in its own words.
var errQuiet = errors.New("failed")

// flags returns a flag set that reports to stderr.
func (a *app) flags(name, args string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	fs.Usage = func() {
		fmt.Fprintf(a.stderr, "usage: heraldo %s %s\n", name, args)
		fs.PrintDefaults()
	}
	return fs
}

// parse parses flags that may come before or after the positional
// arguments, and returns the positional ones.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, errUsage
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// client returns a client for the configured server.
func (a *app) client() (*client.Client, error) {
	u := a.setting(envURL)
	if u == "" {
		return nil, fmt.Errorf("%s is not set: the server root, such as https://heraldo.example.net", envURL)
	}
	tok := a.setting(envToken)
	if tok == "" {
		return nil, fmt.Errorf("%s is not set: a read token for the server", envToken)
	}
	return &client.Client{
		BaseURL: u, Token: tok, WriteToken: a.setting(envWriteToken), HTTP: a.http,
		UserAgent: "heraldo-cli/" + version.String(),
	}, nil
}
