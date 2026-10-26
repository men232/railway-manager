package rotation

import (
	"sort"

	"github.com/men232/railway-manager/internal/domain"
)

// Section is where a row is listed, in display order.
type Section int

const (
	SectionDatabase Section = iota
	SectionGenerated
	SectionPaste
	SectionAdded
)

// Row is what the screen lists: one or more groups sharing names and class.
// Letters are the indexes of Groups (a = 0).
type Row struct {
	// Key is the first group's Key.
	Key, Label string
	Class      Class
	Section    Section
	Groups     []Group

	Public bool
	// Hidden: ClassHidden or Public; the screen decides whether to show it.
	Hidden, OffByDefault   bool
	Warning, Problem, Note string
}

// Letter is the letter of the group at index i: a, b, c…
func Letter(i int) string {
	if i < 26 {
		return string(rune('a' + i))
	}
	return Letter(i/26-1) + Letter(i%26)
}

func sectionOf(c Class) Section {
	switch c {
	case ClassGen:
		return SectionGenerated
	case ClassAsk, ClassHidden:
		return SectionPaste
	default:
		return SectionDatabase
	}
}

// Rows merges GEN, ASK and hidden groups that share any name and the same
// class into one row; DB and URL groups are each their
// own row. A Mongo database's KEYFILE group is left to its password row
// (PasswordRows). Rows are sorted by section, then as the groups were
// ordered.
func Rows(a Analysis) []Row {
	a.Groups = withoutKeyFiles(a)
	n := len(a.Groups)
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	type nameClass struct {
		name  string
		class Class
	}
	seen := map[nameClass]int{}
	for i, g := range a.Groups {
		if g.Class == ClassDB || g.Class == ClassURL {
			continue
		}
		for _, name := range g.Names {
			k := nameClass{name, g.Class}
			if j, ok := seen[k]; ok {
				if ri, rj := find(i), find(j); ri != rj {
					if rj < ri {
						ri, rj = rj, ri
					}
					parent[rj] = ri
				}
			} else {
				seen[k] = i
			}
		}
	}

	byRoot := map[int]*Row{}
	var order []int
	for i, g := range a.Groups {
		r := find(i)
		row, ok := byRoot[r]
		if !ok {
			row = &Row{Key: g.Key, Class: g.Class, Section: sectionOf(g.Class), Public: true}
			byRoot[r] = row
			order = append(order, r)
		}
		row.Groups = append(row.Groups, g)
		row.Public = row.Public && g.Public
		row.OffByDefault = row.OffByDefault || g.OffByDefault
		if row.Warning == "" {
			row.Warning = g.Warning
		}
		if row.Problem == "" {
			row.Problem = g.Problem
		}
		if row.Note == "" {
			row.Note = g.Note
		}
	}
	rows := make([]Row, 0, len(order))
	for _, r := range order {
		row := byRoot[r]
		var names []string
		for _, g := range row.Groups {
			names = append(names, g.Names...)
		}
		row.Label = Label(names)
		row.Hidden = row.Class == ClassHidden || row.Public
		rows = append(rows, *row)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Section < rows[j].Section })
	return rows
}

// withoutKeyFiles drops the groups that are only KEYFILE, live only on
// database places, and hold a Mongo owner's KEYFILE: those are KEYFILE
// password rows.
func withoutKeyFiles(a Analysis) []Group {
	owners := map[string]bool{}
	for _, db := range a.DBs {
		if db.Kind == "mongo" {
			owners[domain.LocationID(db.Owner.EnvID, db.Owner.ServiceID, "KEYFILE")] = true
		}
	}
	out := make([]Group, 0, len(a.Groups))
	for _, g := range a.Groups {
		if !isKeyFileGroup(a, g, owners) {
			out = append(out, g)
		}
	}
	return out
}

func isKeyFileGroup(a Analysis, g Group, owners map[string]bool) bool {
	if len(g.Names) != 1 || g.Names[0] != "KEYFILE" {
		return false
	}
	owner := false
	for _, l := range g.Locs {
		if !a.IsDBPlace(l.Place) {
			return false
		}
		owner = owner || owners[l.ID]
	}
	return owner
}
