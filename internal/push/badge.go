package push

import "github.com/noahkiss/heraldo/internal/settings"

// badgeScope is badge.scope and badge.projects in force: what the badge
// each push carries counts (store.Badge).
func (p *Pusher) badgeScope() (string, []string) {
	m := p.prefs()
	return settings.From[string](m, "badge.scope"), settings.From[[]string](m, "badge.projects")
}
