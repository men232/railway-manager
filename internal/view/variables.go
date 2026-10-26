package view

import (
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/store"
)

// VarRow is one variable at a place; Change and Overlay say what the change
// set does to it.
type VarRow struct {
	LocationID, Name string
	Length           int
	Sealed           bool
	UpdatedAt        string
	Change           *changeset.Change
	Overlay          apply.Overlay
	// Missing marks a pending edit or delete of a variable the store no
	// longer holds (it vanished from Railway after the change was made).
	Missing bool
	// Invalid and Drifted are as ChangeRow's: why the change cannot be
	// applied as it stands, and why its base no longer matches the store
	// (never both).
	Invalid string
	Drifted string
}

func isVariableKind(kind string) bool { return strings.HasPrefix(kind, "variable.") }

// PlaceVariables lists the variables at a place sorted by name: the store's
// plus the ones the change set adds.
func (v *Views) PlaceVariables(cs *changeset.ChangeSet, p domain.Place) []VarRow {
	read := v.inventoryRead(p.ProjectID)
	k := key(append(rev(v.Store, store.Variables), csRev(cs), p.Key(), strconv.FormatBool(read))...)
	return v.vars.Get(k, func() []VarRow {
		return v.placeVars(variableChanges(changes(cs))[p.Key()], p)
	})
}

// variableChanges indexes the variable changes by place key, then target.
func variableChanges(cl []changeset.Change) map[string]map[string]*changeset.Change {
	out := map[string]map[string]*changeset.Change{}
	for i := range cl {
		c := &cl[i]
		if !isVariableKind(c.Kind) {
			continue
		}
		pk := c.Place.Key()
		if out[pk] == nil {
			out[pk] = map[string]*changeset.Change{}
		}
		out[pk][c.Target] = c
	}
	return out
}

// placeVars builds the rows of one place from the store's variables there
// and byName, the place's variable changes by target. Both PlaceVariables
// and Tree read a place through it, so they agree on overlays, Missing,
// Invalid and Drifted.
func (v *Views) placeVars(byName map[string]*changeset.Change, p domain.Place) []VarRow {
	// marks mirrors ChangeSet: an invalid change shows invalid, never
	// drifted.
	marks := func(c *changeset.Change) (invalid, drifted string) {
		if c == nil {
			return "", ""
		}
		if err := v.Reg.CheckData(*c); err != nil {
			return err.Error(), ""
		}
		return "", v.drift(*c)
	}
	var rows []VarRow
	seen := map[string]bool{}
	for _, sv := range v.Store.VariablesAt(p) {
		seen[sv.Name] = true
		c := byName[sv.Name]
		inv, dr := marks(c)
		rows = append(rows, VarRow{
			LocationID: sv.ID, Name: sv.Name, Length: utf8.RuneCountInString(sv.Unrendered),
			Sealed: sv.Sealed, UpdatedAt: sv.UpdatedAt, Change: copyChange(c), Overlay: v.changeOverlay(c),
			Invalid: inv, Drifted: dr,
		})
	}
	for name, c := range byName {
		if seen[name] || (c.Base.Absent && c.Kind == "variable.delete") {
			continue
		}
		inv, dr := marks(c)
		rows = append(rows, VarRow{
			LocationID: domain.LocationID(p.EnvID, p.ServiceID, name), Name: name,
			Change: copyChange(c), Overlay: v.changeOverlay(c), Missing: !c.Base.Absent, Invalid: inv, Drifted: dr,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

// copyChange detaches the row from the change set's backing slice.
func copyChange(c *changeset.Change) *changeset.Change {
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}
