package view

import (
	"sort"
	"strings"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/store"
)

// ChangeSetView is the change set as the review screen shows it. Coverage
// is one entry per rotate group of the pending changes that leaves places
// holding the old value (Changed < Total), sorted by label; add groups and
// other producers' groups never appear.
type ChangeSetView struct {
	Groups    []ChangeGroup
	Redeploys []RedeployRow
	Coverage  []rotation.Coverage
	Err       string
}

// ChangeGroup holds the changes of one service in one environment.
type ChangeGroup struct {
	Label   string
	Changes []ChangeRow
}

// ChangeRow is one change with its overlay. Invalid is why the change
// cannot be applied as it stands (Registry.CheckData: an unknown kind or data
// its kind refuses, as a corrupt change loaded from disk), unsanitized; ""
// when it is valid. Drifted is why the target no longer matches the change's
// base on Railway (see Views.drift), "" when it does, when the project's
// inventory has not been read yet, or when the change is invalid. Both are
// derived, never persisted, and Drifted never blocks apply.
type ChangeRow struct {
	Change  changeset.Change
	Name    string
	Overlay apply.Overlay
	Invalid string
	Drifted string
}

// RedeployRow is one place that will be (or, when !On, would be) redeployed.
type RedeployRow struct {
	PlaceKey, Label string
	On              bool
}

const redeployPrefix = "redeploy:"

// ChangeSet groups the changes by environment label then service, marks the
// invalid ones, and lists the redeploys compiling the unlocked valid changes
// yields. It is recomputed with every change set revision, so the marks
// follow a load, a put and a remove. Coverage counts the locations of
// envIDs, the selected environments.
func (v *Views) ChangeSet(cs *changeset.ChangeSet, envIDs []string) ChangeSetView {
	k := key(append(rev(v.Store, store.Catalog, store.Variables), csRev(cs), v.inventoriesRead(changes(cs)), strings.Join(envIDs, ","))...)
	return v.csView.Get(k, func() ChangeSetView {
		var out ChangeSetView
		if cs == nil {
			return out
		}
		groups := map[string]*ChangeGroup{}
		for _, c := range cs.Changes {
			label := v.Store.EnvLabel(c.Place.EnvID) + "/" + v.Store.ServiceName(c.Place.ServiceID)
			g := groups[label]
			if g == nil {
				g = &ChangeGroup{Label: label}
				groups[label] = g
			}
			var ov apply.Overlay
			if kd, ok := v.Reg[c.Kind]; ok {
				ov = kd.Overlay(c)
			}
			row := ChangeRow{Change: c, Name: c.Target, Overlay: ov}
			if err := v.Reg.CheckData(c); err != nil {
				row.Invalid = err.Error()
			} else {
				row.Drifted = v.drift(c)
			}
			g.Changes = append(g.Changes, row)
		}
		for _, g := range groups {
			out.Groups = append(out.Groups, *g)
		}
		sort.Slice(out.Groups, func(i, j int) bool { return out.Groups[i].Label < out.Groups[j].Label })

		out.Redeploys, out.Err = v.redeploys(cs)
		out.Coverage = v.coverage(cs.Changes, envIDs)
		return out
	})
}

// coverage is the partial rotate groups among the changes, sorted by label
// (then group, for a stable order between equal labels).
func (v *Views) coverage(cl []changeset.Change, envIDs []string) []rotation.Coverage {
	type entry struct {
		group string
		cov   rotation.Coverage
	}
	var out []entry
	seen := map[string]bool{}
	for _, c := range cl {
		if seen[c.Group] {
			continue
		}
		seen[c.Group] = true
		cov, ok := rotation.CoverageOf(v.Store, cl, c.Group, envIDs)
		if ok && cov.Changed < cov.Total {
			out = append(out, entry{c.Group, cov})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].cov.Label != out[j].cov.Label {
			return out[i].cov.Label < out[j].cov.Label
		}
		return out[i].group < out[j].group
	})
	covs := make([]rotation.Coverage, len(out))
	for i, e := range out {
		covs[i] = e.cov
	}
	return covs
}

// drift is why a change's target no longer matches its base in the store
// (the kind's StoreDrift), or "" until the inventory of its project has been
// read: before that the store's absence of a variable means nothing.
func (v *Views) drift(c changeset.Change) string {
	if !v.inventoryRead(c.Place.ProjectID) {
		return ""
	}
	return v.Reg.StoreDrift(v.Store, c)
}

func (v *Views) inventoryRead(projectID string) bool {
	return !v.Store.Load(store.InventoryKey(projectID)).ReadAt.IsZero()
}

// inventoriesRead is a cache key part: which of the changes' projects have
// had their inventory read. Variables data changes bump store.Variables; this
// catches the read landing.
func (v *Views) inventoriesRead(cl []changeset.Change) string {
	seen := map[string]bool{}
	var read []string
	for _, c := range cl {
		p := c.Place.ProjectID
		if seen[p] {
			continue
		}
		seen[p] = true
		if v.inventoryRead(p) {
			read = append(read, p)
		}
	}
	sort.Strings(read)
	return strings.Join(read, ",")
}

func (v *Views) redeploys(cs *changeset.ChangeSet) ([]RedeployRow, string) {
	var rows []RedeployRow
	listed := map[string]bool{}
	var errText string
	if v.Compile != nil {
		unlocked := changeset.ChangeSet{RedeployOff: cs.RedeployOff, Rev: cs.Rev}
		for _, c := range cs.Changes {
			// An invalid change is never applied: it brings no redeploy.
			if c.RunID == "" && v.Reg.CheckData(c) == nil {
				unlocked.Changes = append(unlocked.Changes, c)
			}
		}
		g, err := v.Compile(unlocked)
		if err != nil {
			errText = err.Error()
		}
		for _, sk := range g.Order {
			pk, ok := strings.CutPrefix(sk, redeployPrefix)
			if !ok || listed[pk] {
				continue
			}
			listed[pk] = true
			rows = append(rows, RedeployRow{PlaceKey: pk, Label: v.placeLabel(pk), On: true})
		}
	}
	for _, pk := range cs.RedeployOff {
		if !listed[pk] {
			listed[pk] = true
			rows = append(rows, RedeployRow{PlaceKey: pk, Label: v.placeLabel(pk)})
		}
	}
	return rows, errText
}

// placeLabel turns "envID/serviceID" into "project/env/service".
func (v *Views) placeLabel(placeKey string) string {
	env, svc, _ := strings.Cut(placeKey, "/")
	if svc == "shared" {
		svc = ""
	}
	return v.Store.EnvLabel(env) + "/" + v.Store.ServiceName(svc)
}
