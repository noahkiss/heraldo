package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/client"
)

// ---- settings_read ------------------------------------------------------

type settingsReadIn struct {
	Key    string `json:"key,omitempty" jsonschema:"One key, such as quiet.enabled. Omit for every key."`
	Prefix string `json:"prefix,omitempty" jsonschema:"Only keys that start with this, such as llm. or quiet.."`
}

const settingsReadDesc = `Read heraldo's server settings: quiet hours, digest, stale sweep, flap grouping and the LLM.

Best for: checking what the server does now, before changing it, or why a feature is off.
Each setting has its value, type, default, and source: flag, env, db (written through the API) or default. A key with locked_by is set by a server flag or env var and cannot be changed here.
Returns: the settings. A secret, such as llm.token, shows only set=true or false.
Not for: the owner's app display preferences, which live on their devices.`

func (t *tools) addSettingsRead(s *mcp.Server) {
	addTool(t, s, &mcp.Tool{Name: "settings_read", Description: settingsReadDesc, Annotations: readOnly("Read settings")},
		func(is *jsonschema.Schema) {
			maxLen(is.Properties["key"], 128)
			maxLen(is.Properties["prefix"], 128)
		},
		map[string]*jsonschema.Schema{
			"settings": arr("", obj("", map[string]*jsonschema.Schema{
				"key": str(""), "type": str(""), "value": {Description: "Absent for a secret."},
				"default": {}, "source": str("flag, env, db or default."), "locked_by": str("The flag or env var that sets it."),
				"set": boolean("A secret only."), "enum": arr("", str("")), "description": str(""), "error": str(""),
			})),
		},
		t.settingsRead)
}

func settingOut(s api.Setting) map[string]any {
	o := map[string]any{"key": s.Key, "type": string(s.Type), "source": string(s.Source), "description": s.Description}
	if s.Secret {
		o["set"] = s.Set != nil && *s.Set
	} else {
		o["value"], o["default"] = s.Value, s.Default
	}
	if s.LockedBy != nil {
		o["locked_by"] = *s.LockedBy
	}
	if s.Enum != nil {
		o["enum"] = *s.Enum
	}
	if s.Error != nil {
		o["error"] = *s.Error
	}
	return o
}

func settingLine(s api.Setting) string {
	v := ""
	if s.Secret {
		v = "(not set)"
		if s.Set != nil && *s.Set {
			v = "(set)"
		}
	} else {
		b, _ := json.Marshal(s.Value)
		v = string(b)
	}
	line := fmt.Sprintf("%s = %s [%s]", s.Key, v, s.Source)
	if s.LockedBy != nil {
		line += " locked by " + *s.LockedBy
	}
	return line
}

func (t *tools) settingsRead(ctx context.Context, _ *mcp.CallToolRequest, in settingsReadIn) (result, error) {
	var list []api.Setting
	if in.Key != "" {
		s, err := t.API.Setting(ctx, in.Key)
		if err != nil {
			return result{}, t.apiErr(err, call{what: "setting " + in.Key, lister: "settings_read without key"})
		}
		list = []api.Setting{s}
	} else {
		all, err := t.API.Settings(ctx)
		if err != nil {
			return result{}, t.apiErr(err, call{what: "settings on this server (it may be older than settings)"})
		}
		for _, s := range all {
			if strings.HasPrefix(s.Key, in.Prefix) {
				list = append(list, s)
			}
		}
	}
	out := make([]map[string]any, 0, len(list))
	lines := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, settingOut(s))
		lines = append(lines, settingLine(s))
	}
	if len(lines) == 0 {
		lines = append(lines, "no settings match")
	}
	return result{out: map[string]any{"settings": out}, text: strings.Join(lines, "\n")}, nil
}

// ---- settings_write -----------------------------------------------------

type settingsWriteIn struct {
	Key   string `json:"key" jsonschema:"The key, from settings_read."`
	Value any    `json:"value,omitempty" jsonschema:"The new value in the key's type: bool, number, string, or array of strings. A duration is a string such as 72h; a clock is HH:MM. Omit with unset."`
	Unset bool   `json:"unset,omitempty" jsonschema:"true: remove the stored value, back to the default."`
}

const settingsWriteDesc = `Change one heraldo server setting, or put it back to its default.

Best for: the owner asks to turn on quiet hours, move the digest time, or tune flap grouping or the LLM.
The change takes effect on the server without a restart. Check the key and its type with settings_read first.
Returns: the setting after the change.
Not for: secrets such as llm.token: they would land in this transcript. The owner sets them with heraldo settings set llm.token - and stdin. Not for keys with locked_by: the server's flag or env var wins; say so to the owner.`

func (t *tools) addSettingsWrite(s *mcp.Server) {
	addTool(t, s, &mcp.Tool{Name: "settings_write", Description: settingsWriteDesc,
		Annotations: &mcp.ToolAnnotations{Title: "Change a setting", IdempotentHint: true,
			DestructiveHint: &noAnnot, OpenWorldHint: &noAnnot}},
		func(is *jsonschema.Schema) {
			maxLen(is.Properties["key"], 128)
			is.Properties["key"].MinLength = ptr(1)
			is.Properties["value"] = &jsonschema.Schema{Description: is.Properties["value"].Description}
		},
		map[string]*jsonschema.Schema{
			"setting": obj("", map[string]*jsonschema.Schema{
				"key": str(""), "value": {}, "source": str(""), "locked_by": str(""),
			}),
		},
		t.settingsWrite)
}

func (t *tools) settingsWrite(ctx context.Context, _ *mcp.CallToolRequest, in settingsWriteIn) (result, error) {
	if in.Key == "" {
		return result{}, toolErr("invalid", "give key", "settings_read lists the keys")
	}
	cur, err := t.API.Setting(ctx, in.Key)
	if err != nil {
		return result{}, t.apiErr(err, call{what: "setting " + in.Key, lister: "settings_read"})
	}
	if cur.Secret {
		return result{}, toolErr("secret", in.Key+" is a secret and never goes through MCP",
			"ask the owner to run heraldo settings set "+in.Key+" - and pipe it on stdin")
	}
	if cur.LockedBy != nil {
		return result{}, toolErr("locked_by_env", in.Key+" is set by "+*cur.LockedBy+" on the server",
			"tell the owner; only the server's operator can change it there")
	}
	var s api.Setting
	verb := "set"
	switch {
	case in.Unset && in.Value != nil:
		return result{}, toolErr("invalid", "give value or unset, not both", "")
	case in.Unset:
		verb = "unset"
		s, err = t.API.DeleteSetting(ctx, in.Key)
	case in.Value == nil:
		return result{}, toolErr("invalid", "give value, or unset=true", "")
	default:
		s, err = t.API.PutSetting(ctx, in.Key, in.Value)
	}
	if err != nil {
		return result{}, t.settingsErr(err, in.Key)
	}
	return result{out: map[string]any{"setting": settingOut(s)}, text: verb + ": " + settingLine(s)}, nil
}

func (t *tools) settingsErr(err error, key string) error {
	var ce *client.Error
	if errors.As(err, &ce) {
		switch {
		case ce.Status == http.StatusConflict && ce.Code != "":
			return &ToolError{Code: ce.Code, Status: ce.Status, Message: ce.Message, Hint: "tell the owner"}
		case ce.Status == http.StatusForbidden || ce.Status == http.StatusUnauthorized:
			return &ToolError{Code: "forbidden", Status: ce.Status, Message: "the admin token was refused",
				Hint: "set HERALDO_ADMIN_TOKEN to an admin token and restart the MCP server"}
		}
	}
	return t.apiErr(err, call{write: true, what: "setting " + key, lister: "settings_read"})
}
