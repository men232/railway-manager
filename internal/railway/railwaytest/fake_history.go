package railwaytest

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/men232/railway-manager/internal/railway"
)

// historyPageSize is the page size of environmentHistory and
// environmentPatches (the documents' first: 100).
const historyPageSize = 100

type historyState struct {
	entries map[string][]map[string]any // raw nodes per environment, newest first
	patches map[string][]railway.EnvPatch
	members map[string][]railway.Member
}

func (h *historyState) init() {
	if h.entries == nil {
		h.entries = map[string][]map[string]any{}
		h.patches = map[string][]railway.EnvPatch{}
		h.members = map[string][]railway.Member{}
	}
}

// SetHistory sets an environment's history, newest first. An entry with one
// name has the payload `{name}` (as Railway's Variable entries do), with
// several `{names}`.
func (f *Fake) SetHistory(envID string, entries []railway.HistoryEntry) {
	nodes := make([]map[string]any, len(entries))
	for i, e := range entries {
		var wf, actor any
		if e.WorkflowID != "" {
			wf = e.WorkflowID
		}
		if e.Actor != "" {
			actor = map[string]any{"principal": e.Actor}
		}
		var names any
		switch len(e.Names) {
		case 0:
		case 1:
			names = map[string]any{"name": e.Names[0]}
		default:
			names = map[string]any{"names": slices.Clone(e.Names)}
		}
		nodes[i] = map[string]any{
			"id": e.ID, "object": e.Object, "action": e.Action,
			"createdAt":  e.CreatedAt.UTC().Format(time.RFC3339Nano),
			"serviceIds": slices.Clone(e.ServiceIDs), "payload": names,
			"workflowId": wf, "actor": actor,
		}
	}
	f.SetHistoryRaw(envID, nodes)
}

// SetHistoryRaw sets an environment's history as raw JSON-shaped nodes,
// newest first, served as given (any payload). A node's `createdAt` is
// matched against the `since` filter.
func (f *Fake) SetHistoryRaw(envID string, nodes []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.history.init()
	f.history.entries[envID] = slices.Clone(nodes)
}

// SetPatches sets an environment's patches; environmentPatches answers them
// newest first, 100 per page.
func (f *Fake) SetPatches(envID string, p []railway.EnvPatch) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.history.init()
	sorted := slices.Clone(p)
	slices.SortStableFunc(sorted, func(a, b railway.EnvPatch) int { return b.CreatedAt.Compare(a.CreatedAt) })
	f.history.patches[envID] = sorted
}

// SetMembers sets the members of a project's workspace. A project without
// members set answers a null workspace.
func (f *Fake) SetMembers(projectID string, m []railway.Member) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.history.init()
	f.history.members[projectID] = slices.Clone(m)
}

// page cuts list at the cursor after ("<envID>:<offset>", "" = the start)
// into one page of historyPageSize. A cursor not of envID, not a number or
// beyond the list is a GraphQL error at field.
func page[T any](list []T, envID, after, field string) (items []T, info map[string]any, errs []gqlError) {
	start := 0
	if after != "" {
		env, off, ok := strings.Cut(after, ":")
		n, err := strconv.Atoi(off)
		if !ok || env != envID || err != nil || n < 0 || n > len(list) {
			return nil, nil, []gqlError{{Message: "Invalid cursor", Path: []any{field}}}
		}
		start = n
	}
	end := min(start+historyPageSize, len(list))
	var endCursor any
	if end > start {
		endCursor = envID + ":" + strconv.Itoa(end)
	}
	return list[start:end], map[string]any{"hasNextPage": end < len(list), "endCursor": endCursor}, nil
}

func (f *Fake) historyRead(op string, v map[string]any) (any, []gqlError) {
	f.history.init()
	h := &f.history
	switch op {
	case "EnvironmentHistory":
		envID := str(v["environmentId"])
		var since time.Time
		if s := str(v["since"]); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return nil, []gqlError{{Message: "Invalid since: " + s, Path: []any{"environmentHistory"}}}
			}
			since = t
		}
		var list []map[string]any
		for _, n := range h.entries[envID] {
			at, _ := time.Parse(time.RFC3339Nano, str(n["createdAt"]))
			if since.IsZero() || !at.Before(since) {
				list = append(list, n)
			}
		}
		items, info, errs := page(list, envID, str(v["after"]), "environmentHistory")
		if errs != nil {
			return nil, errs
		}
		conn := edgesOf(items)
		conn["pageInfo"] = info
		return map[string]any{"environmentHistory": conn}, nil
	case "EnvironmentPatches":
		envID := str(v["environmentId"])
		items, info, errs := page(h.patches[envID], envID, str(v["after"]), "environmentPatches")
		if errs != nil {
			return nil, errs
		}
		nodes := make([]map[string]any, 0, len(items))
		for _, p := range items {
			nodes = append(nodes, map[string]any{"id": p.ID, "message": p.Message, "createdAt": p.CreatedAt.UTC().Format(time.RFC3339Nano)})
		}
		conn := edgesOf(nodes)
		conn["pageInfo"] = info
		return map[string]any{"environmentPatches": conn}, nil
	case "ProjectMembers":
		members, ok := h.members[str(v["projectId"])]
		if !ok {
			return map[string]any{"project": map[string]any{"workspace": nil}}, nil
		}
		out := []map[string]any{}
		for _, m := range members {
			var name any
			if m.Name != "" {
				name = m.Name
			}
			out = append(out, map[string]any{"id": m.ID, "name": name, "email": m.Email})
		}
		return map[string]any{"project": map[string]any{"workspace": map[string]any{"members": out}}}, nil
	}
	return nil, []gqlError{{Message: "unknown operation " + op}}
}
