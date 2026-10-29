package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/client"
)

// ---- settings ----------------------------------------------------------

const settingsUsage = `ls [--json] | get [--json] <key> | set <key> <value> | set <secret-key> - | unset <key>`

// settings reads and writes the server's settings. Reads use the read
// token; writes use HERALDO_ADMIN_TOKEN.
func (a *app) settings(ctx context.Context, args []string) error {
	sub := "ls"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "ls", "list":
		return a.settingsLs(ctx, args)
	case "get":
		return a.settingsGet(ctx, args)
	case "set":
		return a.settingsSet(ctx, args)
	case "unset":
		return a.settingsUnset(ctx, args)
	}
	fmt.Fprintf(a.stderr, "usage: heraldo settings %s\n", settingsUsage)
	return errUsage
}

func (a *app) settingsClient() (*client.Client, error) {
	c, err := a.client()
	if err != nil {
		return nil, err
	}
	c.AdminToken = a.setting(envAdminToken)
	return c, nil
}

// settingsErr adds what to do to a refused settings call.
func settingsErr(err error) error {
	switch {
	case client.IsStatus(err, http.StatusNotFound):
		return fmt.Errorf("%w (heraldo settings ls lists the keys; a server before settings has none)", err)
	case client.IsStatus(err, http.StatusForbidden):
		return fmt.Errorf("%w (settings writes need an admin token in %s)", err, envAdminToken)
	}
	return err
}

func (a *app) settingsLs(ctx context.Context, args []string) error {
	fs := a.flags("settings ls", "[--json]")
	asJSON := fs.Bool("json", false, "print each setting as JSON on one line")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) != 0 {
		fs.Usage()
		return errUsage
	}
	c, err := a.settingsClient()
	if err != nil {
		return err
	}
	list, err := c.Settings(ctx)
	if err != nil {
		return settingsErr(err)
	}
	if *asJSON {
		for _, s := range list {
			b, _ := json.Marshal(s)
			fmt.Fprintf(a.stdout, "%s\n", b)
		}
		return nil
	}
	fmt.Fprintf(a.stdout, "%-24s %-24s %-8s %s\n", "KEY", "VALUE", "SOURCE", "LOCKED BY")
	for _, s := range list {
		fmt.Fprintf(a.stdout, "%-24s %-24s %-8s %s\n", s.Key, cutLine(settingText(s), 24), s.Source, str(s.LockedBy))
	}
	return nil
}

// settingText is a setting's value for a person: a secret shows only
// whether it is set.
func settingText(s api.Setting) string {
	if s.Secret {
		if s.Set != nil && *s.Set {
			return "(set)"
		}
		return "(not set)"
	}
	switch v := s.Value.(type) {
	case string:
		if v == "" {
			return `""`
		}
		return oneLine(v)
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func (a *app) settingsGet(ctx context.Context, args []string) error {
	fs := a.flags("settings get", "[--json] <key>")
	asJSON := fs.Bool("json", false, "print the setting as JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return errUsage
	}
	c, err := a.settingsClient()
	if err != nil {
		return err
	}
	s, err := c.Setting(ctx, pos[0])
	if err != nil {
		return settingsErr(err)
	}
	a.printSetting(s, *asJSON)
	return nil
}

func (a *app) printSetting(s api.Setting, asJSON bool) {
	if asJSON {
		b, _ := json.Marshal(s)
		fmt.Fprintf(a.stdout, "%s\n", b)
		return
	}
	fmt.Fprintln(a.stdout, settingText(s))
	note := "source " + string(s.Source)
	if s.LockedBy != nil {
		note += ", locked by " + *s.LockedBy
	}
	if s.Error != nil {
		note += "; " + *s.Error
	}
	fmt.Fprintln(a.stderr, note)
}

func (a *app) settingsSet(ctx context.Context, args []string) error {
	fs := a.flags("settings set", "<key> <value> | <secret-key> -")
	asJSON := fs.Bool("json", false, "print the setting as JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		fs.Usage()
		return errUsage
	}
	key, text := pos[0], pos[1]
	c, err := a.settingsClient()
	if err != nil {
		return err
	}
	cur, err := c.Setting(ctx, key)
	if err != nil {
		return settingsErr(err)
	}
	var value any
	if cur.Secret {
		// Never from argv: ps and shell history would show it.
		if text != "-" {
			return fmt.Errorf("%s is a secret: give - and pipe it on stdin, such as heraldo settings set %s - < file", key, key)
		}
		b, err := io.ReadAll(io.LimitReader(a.stdin, 16<<10))
		if err != nil {
			return err
		}
		v := strings.TrimRight(string(b), "\r\n")
		if v == "" {
			return errors.New("no secret on stdin")
		}
		value = v
	} else if value, err = settingValue(cur, text); err != nil {
		return fmt.Errorf("%s: %v", key, err)
	}
	s, err := c.PutSetting(ctx, key, value)
	if err != nil {
		return settingsErr(err)
	}
	a.printSetting(s, *asJSON)
	return nil
}

// settingValue turns text into the JSON type of s. A list is JSON or
// comma-separated.
func settingValue(s api.Setting, text string) (any, error) {
	switch s.Type {
	case api.SettingTypeBool:
		b, err := strconv.ParseBool(text)
		if err != nil {
			return nil, errors.New("want true or false")
		}
		return b, nil
	case api.SettingTypeInt:
		n, err := strconv.Atoi(text)
		if err != nil {
			return nil, errors.New("want a whole number")
		}
		return n, nil
	case api.SettingTypeList:
		if strings.HasPrefix(strings.TrimSpace(text), "[") {
			var l []string
			if err := json.Unmarshal([]byte(text), &l); err != nil {
				return nil, errors.New("want a JSON array of strings or a comma-separated list")
			}
			return l, nil
		}
		l := []string{}
		for _, f := range strings.Split(text, ",") {
			if f = strings.TrimSpace(f); f != "" {
				l = append(l, f)
			}
		}
		return l, nil
	}
	return text, nil
}

func (a *app) settingsUnset(ctx context.Context, args []string) error {
	fs := a.flags("settings unset", "<key>")
	asJSON := fs.Bool("json", false, "print the setting as JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return errUsage
	}
	c, err := a.settingsClient()
	if err != nil {
		return err
	}
	s, err := c.DeleteSetting(ctx, pos[0])
	if err != nil {
		return settingsErr(err)
	}
	a.printSetting(s, *asJSON)
	return nil
}
