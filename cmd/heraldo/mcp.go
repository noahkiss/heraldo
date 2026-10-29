package main

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/noahkiss/heraldo/internal/mcptools"
	"github.com/noahkiss/heraldo/internal/version"
)

// mcpTransport is the MCP transport; tests replace it.
var mcpTransport = func() mcp.Transport { return &mcp.StdioTransport{} }

// mcp serves the MCP tools on stdio. stdout carries only JSON-RPC; logs go
// to stderr as JSON lines.
func (a *app) mcp(ctx context.Context, args []string) error {
	fs := a.flags("mcp", "[--read-only]")
	readOnly := fs.Bool("read-only", false, "register the read tools only, even with a write token")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		fs.Usage()
		return errUsage
	}
	return a.mcpServer(*readOnly).Run(ctx, mcpTransport())
}

func (a *app) mcpServer(readOnly bool) *mcp.Server {
	log := slog.New(slog.NewJSONHandler(a.stderr, nil))
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".")
	o := mcptools.Options{
		Cursor: a.cursorName(), Source: a.setting(envSource), Project: a.setting(envProject),
		Harness: mcptools.HarnessFromEnv(a.getenv), Host: host, Version: version.String(), Logger: log,
		ConfigHint: "set it in the environment or in " + a.envFilePath(),
	}
	c, err := a.client()
	if err != nil {
		o.NotConfigured = err.Error()
		log.Warn("mcp: not configured", "err", err)
	} else {
		o.API = c
		if u, err := url.Parse(c.BaseURL); err == nil {
			o.URLHost = u.Host
		}
		switch {
		case readOnly:
			o.WriteOff = "--read-only"
		case c.WriteToken == "":
			o.WriteOff = "no " + envWriteToken
		default:
			o.Write = true
		}
		// settings_write needs its own admin token: a write token alone
		// does not list it.
		if c.AdminToken = a.setting(envAdminToken); c.AdminToken != "" && !readOnly {
			o.Admin = true
		}
		c.UserAgent = "heraldo-mcp/" + version.String()
	}
	return mcptools.New(o)
}

// envFilePath is the env file the CLI reads, for messages.
func (a *app) envFilePath() string {
	if p := a.getenv(envEnvFile); p != "" {
		return p
	}
	if d := a.configDir(); d != "" {
		return filepath.Join(d, "env")
	}
	return "$XDG_CONFIG_HOME/heraldo/env"
}
