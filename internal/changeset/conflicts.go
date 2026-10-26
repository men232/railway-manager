package changeset

import (
	"cmp"
	"maps"
	"slices"
)

// Effects reports the targets a change writes and reads (e.g. "var:<locationID>").
// Both are pure functions of the change data.
type Effects interface {
	Writes(c Change) []string
	Reads(c Change) []string
}

// Conflict is one overlap between a candidate change and a change of another set.
type Conflict struct {
	Candidate, Other Change
	OtherRun, Target string
}

// CheckConflicts finds overlaps (writes∩writes, writes∩reads, reads∩writes)
// between candidate and every active set (runID → set; "" is the unselected
// rest). The result is sorted by other run, candidate id, other id, target.
func CheckConflicts(e Effects, candidate ChangeSet, active map[string]ChangeSet) []Conflict {
	var out []Conflict
	for _, runID := range slices.Sorted(maps.Keys(active)) {
		for _, o := range active[runID].Changes {
			ow, or := e.Writes(o), e.Reads(o)
			for _, c := range candidate.Changes {
				cw, cr := e.Writes(c), e.Reads(c)
				seen := map[string]bool{}
				for _, pair := range [][2][]string{{cw, ow}, {cw, or}, {cr, ow}} {
					for _, t := range pair[0] {
						if slices.Contains(pair[1], t) && !seen[t] {
							seen[t] = true
							out = append(out, Conflict{Candidate: c, Other: o, OtherRun: runID, Target: t})
						}
					}
				}
			}
		}
	}
	slices.SortStableFunc(out, func(a, b Conflict) int {
		return cmp.Or(
			cmp.Compare(a.OtherRun, b.OtherRun),
			cmp.Compare(a.Candidate.ID, b.Candidate.ID),
			cmp.Compare(a.Other.ID, b.Other.ID),
			cmp.Compare(a.Target, b.Target),
		)
	})
	return out
}
