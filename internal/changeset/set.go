package changeset

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// skippedFlagPrefix starts the Flag a run gives a change it skipped because
// its target drifted (apply's "skipped: <reason>").
const skippedFlagPrefix = "skipped:"

// ErrLocked means a run holds the change.
var ErrLocked = errors.New("change is held by a run")

// ChangeSet is an ordered list of changes plus per-place redeploy opt-outs.
// Every change to it goes through a method here, which bumps Rev: a copy
// (Clone) checked off the UI goroutine is still current while Rev is equal.
type ChangeSet struct {
	Changes     []Change `json:"changes"`
	RedeployOff []string `json:"redeployOff,omitempty"` // place keys
	Rev         uint64   `json:"-"`
	// Now is the clock for Created on new changes; time.Now when nil.
	Now func() time.Time `json:"-"`
}

func (s *ChangeSet) index(id string) int {
	return slices.IndexFunc(s.Changes, func(c Change) bool { return c.ID == id })
}

// Put adds c, or merges it into the change with the same Key(): the newer
// replaces the older but keeps the older's Base, ID and Created. When the
// older carries a "skipped:" flag (a run found its target drifted), the newer
// keeps its own Base, since the user re-edited against what Railway holds
// now, and the flag is cleared. A newer change with neither a Group nor an
// Origin (an edit made outside the producer, e.g. in Variables) keeps the
// older's, so it stays in its group. In a database password group
// (PasswordGroupPrefix) such an edit with no Requires also keeps the older's
// Requires, so it still waits for the password change, and an automatic
// rewrite (Origin.Letter LetterRewrite) edited so becomes a decision: the
// Letter is cleared. The merged change is removed when isNoop reports it is
// back at its base.
func (s *ChangeSet) Put(c Change, isNoop func(Change) bool) error {
	i := slices.IndexFunc(s.Changes, func(o Change) bool { return o.Key() == c.Key() })
	if i >= 0 {
		old := s.Changes[i]
		if old.RunID != "" {
			return fmt.Errorf("%s: %w", c.Key(), ErrLocked)
		}
		c.ID, c.Created = old.ID, old.Created
		if strings.HasPrefix(old.Flag, skippedFlagPrefix) {
			c.Flag = ""
		} else {
			c.Base = old.Base
		}
		if c.Group == "" && c.Origin == nil {
			edit := len(c.Requires) == 0
			c.Group, c.Origin = old.Group, old.Origin
			if edit && strings.HasPrefix(old.Group, PasswordGroupPrefix) {
				c.Requires = slices.Clone(old.Requires)
				if o := old.Origin; o != nil && o.Letter == LetterRewrite {
					by := *o
					by.Letter = ""
					c.Origin = &by
				}
			}
		}
		c.RunID = ""
	} else if c.ID == "" {
		c.ID = NewID()
	}
	if c.Created.IsZero() {
		if s.Now != nil {
			c.Created = s.Now()
		} else {
			c.Created = time.Now()
		}
	}
	s.Rev++
	switch {
	case isNoop != nil && isNoop(c):
		if i >= 0 {
			s.Changes = slices.Delete(s.Changes, i, i+1)
		}
	case i >= 0:
		s.Changes[i] = c
	default:
		s.Changes = append(s.Changes, c)
	}
	return nil
}

// Remove deletes the change; ErrLocked when a run holds it.
func (s *ChangeSet) Remove(id string) error {
	i := s.index(id)
	if i < 0 {
		return fmt.Errorf("change %s not found", id)
	}
	if s.Changes[i].RunID != "" {
		return fmt.Errorf("%s: %w", s.Changes[i].Key(), ErrLocked)
	}
	s.Changes = slices.Delete(s.Changes, i, i+1)
	s.Rev++
	return nil
}

// Get returns the change with the id.
func (s *ChangeSet) Get(id string) (Change, bool) {
	if i := s.index(id); i >= 0 {
		return s.Changes[i], true
	}
	return Change{}, false
}

// Select partitions the set by id. Both parts carry a copy of RedeployOff.
// Refusing ids held by a run is the caller's concern.
func (s *ChangeSet) Select(ids []string) (selected, rest ChangeSet) {
	selected.RedeployOff = slices.Clone(s.RedeployOff)
	rest.RedeployOff = slices.Clone(s.RedeployOff)
	for _, c := range s.Changes {
		if slices.Contains(ids, c.ID) {
			selected.Changes = append(selected.Changes, c)
		} else {
			rest.Changes = append(rest.Changes, c)
		}
	}
	return selected, rest
}

// ToggleRedeploy flips the redeploy opt-out for a place key.
func (s *ChangeSet) ToggleRedeploy(placeKey string) {
	if i := slices.Index(s.RedeployOff, placeKey); i >= 0 {
		s.RedeployOff = slices.Delete(s.RedeployOff, i, i+1)
	} else {
		s.RedeployOff = append(s.RedeployOff, placeKey)
	}
	s.Rev++
}

// Lock marks the changes as held by the run and clears their old flag. It
// changes nothing and returns ErrLocked if any is held by a different run.
func (s *ChangeSet) Lock(ids []string, runID string) error {
	for _, c := range s.Changes {
		if slices.Contains(ids, c.ID) && c.RunID != "" && c.RunID != runID {
			return fmt.Errorf("%s: %w", c.Key(), ErrLocked)
		}
	}
	for i := range s.Changes {
		if slices.Contains(ids, s.Changes[i].ID) {
			s.Changes[i].RunID, s.Changes[i].Flag = runID, ""
		}
	}
	s.Rev++
	return nil
}

// Unlock releases the run's changes; flags (change id → reason) set Flag.
func (s *ChangeSet) Unlock(runID string, flags map[string]string) {
	for i := range s.Changes {
		if s.Changes[i].RunID == runID {
			s.Changes[i].RunID = ""
			if f, ok := flags[s.Changes[i].ID]; ok {
				s.Changes[i].Flag = f
			}
		}
	}
	s.Rev++
}

// UnlockChanges releases the changes ids that the run holds, and no other;
// flags (change id → reason) set Flag.
func (s *ChangeSet) UnlockChanges(runID string, ids []string, flags map[string]string) {
	for i := range s.Changes {
		c := &s.Changes[i]
		if c.RunID == runID && slices.Contains(ids, c.ID) {
			c.RunID = ""
			if f, ok := flags[c.ID]; ok {
				c.Flag = f
			}
		}
	}
	s.Rev++
}

// Drop removes the changes regardless of lock (a run finished with them).
func (s *ChangeSet) Drop(ids []string) {
	s.Changes = slices.DeleteFunc(s.Changes, func(c Change) bool { return slices.Contains(ids, c.ID) })
	s.Rev++
}

// Flag sets the flags (change id → reason) of the run's changes, which stay
// held by it.
func (s *ChangeSet) Flag(runID string, flags map[string]string) {
	for i := range s.Changes {
		if f, ok := flags[s.Changes[i].ID]; ok && s.Changes[i].RunID == runID {
			s.Changes[i].Flag = f
		}
	}
	s.Rev++
}

// Clone returns a deep copy, Rev included, that shares no memory with s: a
// command can read it while s is changed.
func (s *ChangeSet) Clone() ChangeSet {
	out := ChangeSet{RedeployOff: slices.Clone(s.RedeployOff), Rev: s.Rev, Now: s.Now}
	if s.Changes != nil {
		out.Changes = make([]Change, len(s.Changes))
		for i, c := range s.Changes {
			c.New = slices.Clone(c.New)
			c.Requires = slices.Clone(c.Requires)
			if c.Origin != nil {
				o := *c.Origin
				c.Origin = &o
			}
			out.Changes[i] = c
		}
	}
	return out
}
