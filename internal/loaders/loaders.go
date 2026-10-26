// Package loaders are the background reads: each is a tea.Cmd returning a
// message. Messages that carry store data implement store.Update, so the
// root model applies them without knowing their type.
package loaders

import (
	"context"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/settings"
	"github.com/men232/railway-manager/internal/store"
)

// inventoryConcurrency is how many projects are discovered at once.
const inventoryConcurrency = 8

// AccountMsg is the result of WhoAmI.
type AccountMsg struct {
	Account railway.Account
	Err     error
}

// CatalogMsg is the result of Catalog.
type CatalogMsg struct {
	Catalog railway.Catalog
	Err     error
	At      time.Time
}

// ApplyTo replaces the catalog, or records why it could not be read; the
// last catalog is kept then.
func (m CatalogMsg) ApplyTo(s *store.Store) {
	if m.Err != nil {
		s.FailLoad(store.CatalogKey, m.Err.Error())
		return
	}
	s.ReplaceCatalog(m.Catalog)
	s.FinishLoad(store.CatalogKey, m.At)
}

// InventoryMsg is the discovery result of one project. Seq is the request
// sequence the caller passed to Inventory, so a late answer to an older
// request can be told apart.
type InventoryMsg struct {
	ProjectID string
	Seq       uint64
	Result    railway.DiscoveryResult
	Err       error
	At        time.Time
}

// ApplyTo replaces the project's variables, or records why they could not
// be read; the last variables are kept then. A partial read keeps the last
// variables of the places it could not read and finishes with its warnings,
// so nothing (Rotate's coverage least of all) takes it for a whole one.
func (m InventoryMsg) ApplyTo(s *store.Store) {
	key := store.InventoryKey(m.ProjectID)
	if m.Err != nil {
		s.FailLoad(key, m.Err.Error())
		return
	}
	vars, system := m.Result.Vars, m.Result.System
	if len(m.Result.Unread) > 0 {
		unread := func(envID, serviceID string) bool {
			return slices.ContainsFunc(m.Result.Unread, func(u railway.UnreadPlace) bool {
				return u.EnvID == envID && u.ServiceID == serviceID
			})
		}
		vars = slices.DeleteFunc(slices.Clone(vars), func(r railway.VarRecord) bool { return unread(r.EnvID, r.ServiceID) })
		system = slices.DeleteFunc(slices.Clone(system), func(r railway.SystemRecord) bool { return unread(r.EnvID, r.ServiceID) })
		kv, ks := s.Known(m.ProjectID, unread)
		vars, system = append(vars, kv...), append(system, ks...)
	}
	s.ReplaceVariables(m.ProjectID, vars, system...)
	if len(m.Result.Unread) > 0 {
		why := make([]string, len(m.Result.Unread))
		for i, u := range m.Result.Unread {
			why[i] = u.Why
		}
		s.FinishLoadWarn(key, m.At, strings.Join(why, "; "))
		return
	}
	s.FinishLoad(key, m.At)
}

var (
	_ store.Update = CatalogMsg{}
	_ store.Update = InventoryMsg{}
)

// WhoAmI reads the signed-in account.
func WhoAmI(c *railway.Client) tea.Cmd {
	return func() tea.Msg {
		a, err := c.WhoAmI(context.Background())
		return AccountMsg{Account: a, Err: err}
	}
}

// Catalog reads every project the account can see; now stamps the result.
func Catalog(c *railway.Client, now func() time.Time) tea.Cmd {
	return func() tea.Msg {
		cat, err := c.ListProjects(context.Background())
		return CatalogMsg{Catalog: cat, Err: err, At: now()}
	}
}

// Inventory discovers the targets: one InventoryMsg per project carrying
// seq, at most 8 projects at a time; now stamps the results. Nil for no
// targets.
func Inventory(c *railway.Client, targets []railway.DiscoveryTarget, seq uint64, now func() time.Time) tea.Cmd {
	sem := make(chan struct{}, inventoryConcurrency)
	cmds := make([]tea.Cmd, 0, len(targets))
	for _, t := range targets {
		cmds = append(cmds, func() tea.Msg {
			sem <- struct{}{}
			defer func() { <-sem }()
			res, err := c.Discover(context.Background(), t)
			return InventoryMsg{ProjectID: t.ProjectID, Seq: seq, Result: res, Err: err, At: now()}
		})
	}
	return tea.Batch(cmds...)
}

// Targets are the discovery targets of the selected projects that exist in
// the catalog, in catalog order. A project's environments are those listed
// in set.Envs, or all of them when none of its environments is listed.
func Targets(projects []railway.Project, set *settings.Settings) []railway.DiscoveryTarget {
	return targets(projects, set, false)
}

// LoadTargets are the targets to read: every environment of the selected
// projects, so a database in an unselected environment can be recognised.
// Screens show Targets only.
func LoadTargets(projects []railway.Project, set *settings.Settings) []railway.DiscoveryTarget {
	return targets(projects, set, true)
}

func targets(projects []railway.Project, set *settings.Settings, all bool) []railway.DiscoveryTarget {
	if set == nil {
		return nil
	}
	var out []railway.DiscoveryTarget
	for _, p := range projects {
		if !slices.Contains(set.Projects, p.ID) {
			continue
		}
		envs := make([]railway.Environment, 0, len(p.Environments))
		for _, e := range p.Environments {
			if slices.Contains(set.Envs, e.ID) {
				envs = append(envs, e)
			}
		}
		if all || len(envs) == 0 {
			envs = p.Environments
		}
		names := make(map[string]string, len(p.Services))
		for _, s := range p.Services {
			names[s.ID] = s.Name
		}
		out = append(out, railway.DiscoveryTarget{
			ProjectID: p.ID, ProjectName: p.Name, Environments: envs, ServiceNames: names,
		})
	}
	return out
}
