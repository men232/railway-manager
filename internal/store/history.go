package store

import (
	"maps"
	"slices"

	"github.com/men232/railway-manager/internal/railway"
)

// envHistory is the history read so far of one environment, newest first.
type envHistory struct {
	entries   []railway.HistoryEntry
	endCursor string
	hasNext   bool
}

// historyData holds the History, Members and Patches collections. Everything
// it holds is owned by the store: values are copied in on write and
// deep-copied by clone.
type historyData struct {
	history map[string]*envHistory                 // env -> feed
	members map[string]map[string]railway.Member   // project -> member id -> member
	patches map[string]map[string]railway.EnvPatch // env -> patch id -> patch
}

func newHistoryData() historyData {
	return historyData{
		history: map[string]*envHistory{},
		members: map[string]map[string]railway.Member{},
		patches: map[string]map[string]railway.EnvPatch{},
	}
}

func cloneEntry(e railway.HistoryEntry) railway.HistoryEntry {
	e.ServiceIDs = slices.Clone(e.ServiceIDs)
	e.Names = slices.Clone(e.Names)
	return e
}

func (h historyData) clone() historyData {
	c := newHistoryData()
	for env, eh := range h.history {
		cp := *eh
		cp.entries = make([]railway.HistoryEntry, len(eh.entries))
		for i, e := range eh.entries {
			cp.entries[i] = cloneEntry(e)
		}
		c.history[env] = &cp
	}
	for p, m := range h.members {
		c.members[p] = maps.Clone(m)
	}
	for env, m := range h.patches {
		c.patches[env] = maps.Clone(m)
	}
	return c
}

// AppendHistory adds one page of an environment's history read from cursor
// (the page's `after`; "" = the first page). A first page replaces the
// environment's entries. A next page is kept only when cursor is the stored
// end cursor; any other page is stale (another read got there first, or the
// feed was restarted) and is dropped. Entries already held are skipped by ID.
func (s *Store) AppendHistory(envID, cursor string, p railway.HistoryPage) {
	eh := s.hist.history[envID]
	switch {
	case cursor == "":
		eh = &envHistory{}
		s.hist.history[envID] = eh
	case eh == nil || cursor != eh.endCursor:
		return
	}
	seen := make(map[string]bool, len(eh.entries)+len(p.Entries))
	for _, e := range eh.entries {
		seen[e.ID] = true
	}
	for _, e := range p.Entries {
		if seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		eh.entries = append(eh.entries, cloneEntry(e))
	}
	eh.endCursor, eh.hasNext = p.EndCursor, p.HasNext
	s.bump(History)
}

// History returns the history read so far of an environment, newest first,
// with the cursor and flag for the next page; read is false until a first
// page was stored. The entries are the store's own: don't change them.
func (s *Store) History(envID string) (entries []railway.HistoryEntry, endCursor string, hasNext, read bool) {
	eh := s.hist.history[envID]
	if eh == nil {
		return nil, "", false, false
	}
	return eh.entries, eh.endCursor, eh.hasNext, true
}

// SetMembers replaces the members of a project.
func (s *Store) SetMembers(projectID string, m []railway.Member) {
	byID := make(map[string]railway.Member, len(m))
	for _, mb := range m {
		byID[mb.ID] = mb
	}
	s.hist.members[projectID] = byID
	s.bump(Members)
}

// MemberName names a principal of a project: the member's name, else the
// email, else the first 8 characters of the id (members not read, or the
// principal isn't one).
func (s *Store) MemberName(projectID, principal string) string {
	if mb, ok := s.hist.members[projectID][principal]; ok {
		if mb.Name != "" {
			return mb.Name
		}
		if mb.Email != "" {
			return mb.Email
		}
	}
	if len(principal) > 8 {
		return principal[:8]
	}
	return principal
}

// SetPatches replaces the patches of an environment.
func (s *Store) SetPatches(envID string, p []railway.EnvPatch) {
	byID := make(map[string]railway.EnvPatch, len(p))
	for _, ep := range p {
		byID[ep.ID] = ep
	}
	s.hist.patches[envID] = byID
	s.bump(Patches)
}

// PatchMessage returns the message of an environment's patch; false when the
// patch isn't held.
func (s *Store) PatchMessage(envID, patchID string) (string, bool) {
	ep, ok := s.hist.patches[envID][patchID]
	return ep.Message, ok
}
