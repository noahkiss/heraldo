package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/api"
)

const guidanceUsage = `[ls] [--scope s] [--json] | add [--disabled] <scope> <text> | edit [--scope s] [--text t] [--enable|--disable] <id> | rm <id>`

// guidance lists, adds, edits or removes guidance lines.
func (a *app) guidance(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "add":
			return a.guidanceAdd(ctx, args[1:])
		case "edit":
			return a.guidanceEdit(ctx, args[1:])
		case "rm":
			return a.guidanceRm(ctx, args[1:])
		case "ls":
			args = args[1:]
		}
	}
	fs := a.flags("guidance", guidanceUsage)
	scope := fs.String("scope", "", "only lines with exactly this scope")
	asJSON := fs.Bool("json", false, "print each line as JSON on one line")
	pos, err := parse(fs, args)
	if err := noPositional(fs, pos, err); err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	gs, err := c.Guidance(ctx, *scope)
	if err != nil {
		return err
	}
	if *asJSON {
		for _, g := range gs {
			a.printJSON(g)
		}
		return nil
	}
	if len(gs) == 0 {
		fmt.Fprintln(a.stderr, "no guidance")
		return nil
	}
	fmt.Fprintf(a.stdout, "%-30s %-24s %-3s %s\n", "ID", "SCOPE", "ON", "TEXT")
	for _, g := range gs {
		on := "yes"
		if !g.Enabled {
			on = "no"
		}
		fmt.Fprintf(a.stdout, "%-30s %-24s %-3s %s\n", g.Id, cutLine(oneLine(g.Scope), 24), on, cutLine(oneLine(g.Text), 80))
	}
	return nil
}

func (a *app) guidanceAdd(ctx context.Context, args []string) error {
	fs := a.flags("guidance add", "[--disabled] <scope> <text>\n\nscope is global, project:<p>, source:<s> or key:<regex>.")
	disabled := fs.Bool("disabled", false, "add the line turned off")
	asJSON := fs.Bool("json", false, "print the line as JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		fs.Usage()
		return errUsage
	}
	c, err := a.writeNeeded("adding guidance")
	if err != nil {
		return err
	}
	on := !*disabled
	g, err := c.AddGuidance(ctx, api.GuidanceInput{Scope: pos[0], Text: strings.Join(pos[1:], " "), Enabled: &on})
	if err != nil {
		return err
	}
	if *asJSON {
		a.printJSON(g)
		return nil
	}
	fmt.Fprintf(a.stdout, "added guidance for %s\n%s\n", g.Scope, g.Id)
	return nil
}

func (a *app) guidanceEdit(ctx context.Context, args []string) error {
	fs := a.flags("guidance edit", "[--scope s] [--text t] [--enable|--disable] <id>")
	scope := fs.String("scope", "", "a new scope")
	text := fs.String("text", "", "new text")
	enable := fs.Bool("enable", false, "turn the line on")
	disable := fs.Bool("disable", false, "turn the line off")
	asJSON := fs.Bool("json", false, "print the line as JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || (*enable && *disable) {
		fs.Usage()
		return errUsage
	}
	var in api.GuidancePatch
	if *scope != "" {
		in.Scope = scope
	}
	if *text != "" {
		in.Text = text
	}
	if *enable || *disable {
		on := *enable
		in.Enabled = &on
	}
	if in.Scope == nil && in.Text == nil && in.Enabled == nil {
		return fmt.Errorf("give --scope, --text, --enable or --disable")
	}
	c, err := a.writeNeeded("editing guidance")
	if err != nil {
		return err
	}
	g, err := c.PutGuidance(ctx, pos[0], in)
	if err != nil {
		return err
	}
	if *asJSON {
		a.printJSON(g)
		return nil
	}
	fmt.Fprintf(a.stdout, "updated %s to version %d\n", g.Id, g.Version)
	return nil
}

func (a *app) guidanceRm(ctx context.Context, args []string) error {
	fs := a.flags("guidance rm", "<id>")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return errUsage
	}
	c, err := a.writeNeeded("removing guidance")
	if err != nil {
		return err
	}
	if err := c.DeleteGuidance(ctx, pos[0]); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "removed guidance %s\n", pos[0])
	return nil
}

const llmUsage = `status [--json] | test`

// llm shows the LLM's status or tests the endpoint.
func (a *app) llm(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	switch args[0] {
	case "status":
		return a.llmStatus(ctx, args[1:])
	case "test":
		return a.llmTest(ctx, args[1:])
	}
	fmt.Fprintf(a.stderr, "usage: heraldo llm %s\n", llmUsage)
	return errUsage
}

func (a *app) llmStatus(ctx context.Context, args []string) error {
	fs := a.flags("llm status", "[--json]")
	asJSON := fs.Bool("json", false, "print the status as JSON")
	pos, err := parse(fs, args)
	if err := noPositional(fs, pos, err); err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	s, err := c.LLMStatus(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		a.printJSON(s)
		return nil
	}
	str := func(p *string) string {
		if p == nil || *p == "" {
			return "-"
		}
		return *p
	}
	mode := string(s.Mode)
	if s.EffectiveMode != api.LlmStatusEffectiveMode(s.Mode) {
		mode += " (in force: " + string(s.EffectiveMode) + ")"
	}
	fmt.Fprintf(a.stdout, "mode       %s\n", mode)
	fmt.Fprintf(a.stdout, "chat       %v\n", s.Chat)
	fmt.Fprintf(a.stdout, "endpoint   %s (%s), token set: %v\n", str(s.BaseHost), str(s.Api), s.TokenSet)
	fmt.Fprintf(a.stdout, "models     chat %s, decisions %s\n", str(s.ChatModel), str(s.DecisionsModel))
	circuit := string(s.Circuit)
	if s.CircuitUntil != nil {
		circuit += " until " + localTime(*s.CircuitUntil)
	}
	fmt.Fprintf(a.stdout, "circuit    %s\n", circuit)
	fmt.Fprintf(a.stdout, "budget     %d of %d tokens today, %d of %d calls this minute\n",
		s.TokensToday, s.TokensPerDay, s.CallsLastMinute, s.CallsPerMinute)
	if s.ShadowUntil != nil {
		fmt.Fprintf(a.stdout, "shadow     until %s (a budget was passed)\n", localTime(*s.ShadowUntil))
	}
	if s.LastError != nil {
		at := ""
		if s.LastErrorAt != nil {
			at = localTime(*s.LastErrorAt) + " "
		}
		fmt.Fprintf(a.stdout, "last error %s%s\n", at, oneLine(*s.LastError))
	}
	if s.Standby != nil && *s.Standby {
		fmt.Fprintln(a.stdout, "standby    yes: this server never calls the LLM")
	}
	return nil
}

func (a *app) llmTest(ctx context.Context, args []string) error {
	fs := a.flags("llm test", "")
	pos, err := parse(fs, args)
	if err := noPositional(fs, pos, err); err != nil {
		return err
	}
	c, err := a.settingsClient()
	if err != nil {
		return err
	}
	r, err := c.LLMTest(ctx)
	if err != nil {
		return settingsErr(err)
	}
	ms := int64(0)
	if r.LatencyMs != nil {
		ms = *r.LatencyMs
	}
	model := "-"
	if r.Model != nil {
		model = *r.Model
	}
	if !r.Ok {
		msg := "no answer"
		if r.Error != nil {
			msg = *r.Error
		}
		return fmt.Errorf("the LLM test failed (%s): %s", model, msg)
	}
	answer := ""
	if r.Answer != nil {
		answer = oneLine(*r.Answer)
	}
	fmt.Fprintf(a.stdout, "ok: %s answered %q in %s\n", model, answer, (time.Duration(ms) * time.Millisecond).String())
	return nil
}

// ask asks the chat model a question.
func (a *app) ask(ctx context.Context, args []string) error {
	fs := a.flags("ask", `[--project p] [--key k] [--view v] [--since 12h] [--json] "question"`)
	project := fs.String("project", "", "ask about this project")
	key := fs.String("key", "", "ask about one incident or job")
	view := fs.String("view", "", "ask about a saved view")
	since := fs.String("since", "", "ask about events since this time: RFC 3339, or a duration such as 12h")
	asJSON := fs.Bool("json", false, "print the answer as JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		fs.Usage()
		return errUsage
	}
	in := api.AskInput{Question: strings.Join(pos, " ")}
	if *project != "" || *key != "" || *view != "" || *since != "" {
		in.Scope = &api.AskScope{Project: optStr(*project), Key: optStr(*key), View: optStr(*view), Since: optStr(*since)}
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	ans, err := c.Ask(ctx, in)
	if err != nil {
		return err
	}
	if *asJSON {
		a.printJSON(ans)
		return nil
	}
	fmt.Fprintln(a.stdout, ans.Answer)
	if len(ans.CitedIds) > 0 {
		fmt.Fprintln(a.stdout, "\ncited:")
		for _, id := range ans.CitedIds {
			fmt.Fprintf(a.stdout, "  %s\n", id)
		}
	}
	if p := ans.Proposal; p != nil {
		summary := ""
		if p.Summary != nil {
			summary = ": " + *p.Summary
		}
		fmt.Fprintf(a.stdout, "\nproposal (%s)%s\n", p.Type, summary)
		fmt.Fprintf(a.stdout, "  %s\n", proposalHint(*p))
	}
	return nil
}

// proposalHint says how to apply a proposal from the CLI. Chat never
// applies it.
func proposalHint(p api.AskProposal) string {
	switch p.Type {
	case api.AskProposalTypeFilter:
		if p.Filter != nil {
			return "heraldo ls " + filterFlags(*p.Filter)
		}
	case api.AskProposalTypeView:
		if p.View != nil {
			f := ""
			if p.View.Filter != nil {
				f = filterFlags(*p.View.Filter) + " "
			}
			return fmt.Sprintf("heraldo views save %s%q", f, p.View.Name)
		}
	case api.AskProposalTypeGuidance:
		if p.Guidance != nil {
			return fmt.Sprintf("heraldo guidance add %q %q", p.Guidance.Scope, p.Guidance.Text)
		}
	case api.AskProposalTypeMute:
		if m := p.Mute; m != nil {
			var parts []string
			for _, kv := range []struct {
				flag string
				v    *string
			}{{"--source", m.Source}, {"--key", m.Key}, {"--project", m.Project}, {"--for", m.For}} {
				if kv.v != nil && *kv.v != "" {
					parts = append(parts, kv.flag+" "+*kv.v)
				}
			}
			return "heraldo mute " + strings.Join(parts, " ")
		}
	}
	return "(no details)"
}

// filterFlags is a view filter as heraldo ls flags.
func filterFlags(f api.ViewFilter) string {
	var parts []string
	add := func(flag string, v *string) {
		if v != nil && *v != "" {
			parts = append(parts, flag+" "+*v)
		}
	}
	if f.Project != nil {
		parts = append(parts, "--project "+strings.Join(*f.Project, ","))
	}
	add("--source", f.Source)
	if f.Kind != nil {
		parts = append(parts, "--kind "+string(*f.Kind))
	}
	add("--host", f.Host)
	add("--tag", f.Tag)
	add("-q", f.Q)
	return strings.Join(parts, " ")
}
