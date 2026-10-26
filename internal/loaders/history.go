package loaders

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
)

// HistorySince is how far back History and Patches read: callers pass
// now - HistorySince as since.
const HistorySince = 30 * 24 * time.Hour

// HistoryMsg is one page of an environment's history read from cursor After
// ("" = the first page).
type HistoryMsg struct {
	EnvID, After string
	Page         railway.HistoryPage
	Err          error
	At           time.Time
}

// ApplyTo adds the page (the store drops a stale one); on failure the
// entries read so far are kept.
func (m HistoryMsg) ApplyTo(s *store.Store) {
	fail, warn := partial(m.Err)
	if finish(s, store.HistoryKey(m.EnvID), fail, warn, m.At) {
		s.AppendHistory(m.EnvID, m.After, m.Page)
	}
}

// MembersMsg is the members of a project (its workspace's).
type MembersMsg struct {
	ProjectID string
	Members   []railway.Member
	Err       error
	At        time.Time
}

// ApplyTo replaces the members; on failure the last are kept.
func (m MembersMsg) ApplyTo(s *store.Store) {
	fail, warn := partial(m.Err)
	if finish(s, store.MembersKey(m.ProjectID), fail, warn, m.At) {
		s.SetMembers(m.ProjectID, m.Members)
	}
}

// PatchesMsg is the committed patches of an environment.
type PatchesMsg struct {
	EnvID   string
	Patches []railway.EnvPatch
	Err     error
	At      time.Time
}

// ApplyTo replaces the patches; on failure the last are kept.
func (m PatchesMsg) ApplyTo(s *store.Store) {
	fail, warn := partial(m.Err)
	if finish(s, store.PatchesKey(m.EnvID), fail, warn, m.At) {
		s.SetPatches(m.EnvID, m.Patches)
	}
}

var (
	_ store.Update = HistoryMsg{}
	_ store.Update = MembersMsg{}
	_ store.Update = PatchesMsg{}
)

// History reads one page of an environment's history from cursor after
// ("" = the first page), entries since since.
func History(c *railway.Client, envID, after string, since time.Time, now func() time.Time) tea.Cmd {
	return func() tea.Msg {
		p, err := c.EnvironmentHistory(context.Background(), envID, after, since)
		return HistoryMsg{EnvID: envID, After: after, Page: p, Err: err, At: now()}
	}
}

// Members reads the members of a project's workspace.
func Members(c *railway.Client, projectID string, now func() time.Time) tea.Cmd {
	return func() tea.Msg {
		m, err := c.ProjectMembers(context.Background(), projectID)
		return MembersMsg{ProjectID: projectID, Members: m, Err: err, At: now()}
	}
}

// Patches reads an environment's patches since since.
func Patches(c *railway.Client, envID string, since time.Time, now func() time.Time) tea.Cmd {
	return func() tea.Msg {
		p, err := c.EnvironmentPatches(context.Background(), envID, since)
		return PatchesMsg{EnvID: envID, Patches: p, Err: err, At: now()}
	}
}
