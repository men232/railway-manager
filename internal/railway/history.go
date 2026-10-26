package railway

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// HistoryEntry is one entry of an environment's history. It never carries a
// value: of the entry's payload only a Variable entry's names are kept; every
// other object has nil Names. Strings are
// as Railway sent them; sanitize them when rendering.
type HistoryEntry struct {
	ID, Object, Action string
	CreatedAt          time.Time
	ServiceIDs, Names  []string
	Actor              string // actor.principal
	// WorkflowID is `commitChanges/<envId>/<patchId>` on an applied
	// EnvironmentPatch entry.
	WorkflowID string
}

// HistoryPage is one page of history, newest first.
type HistoryPage struct {
	Entries   []HistoryEntry
	EndCursor string
	HasNext   bool
}

// EnvPatch is one committed patch of an environment.
type EnvPatch struct {
	ID, Message string
	CreatedAt   time.Time
}

// Member is one member of a project.
type Member struct{ ID, Name, Email string }

// payloadNames decodes a history payload keeping only the variable names:
// `names` (a list) or else `name` (one string; Variable entries carry this).
// Every other field is skipped by the decoder. `names` and `name` are read
// apart, so a bad one doesn't lose the other; a field of another type gives
// nothing rather than an error, so one odd entry can't fail the page.
type payloadNames []string

func (p *payloadNames) UnmarshalJSON(b []byte) error {
	*p = nil
	var list struct {
		Names []string `json:"names"`
	}
	if json.Unmarshal(b, &list) == nil && len(list.Names) > 0 {
		*p = list.Names
		return nil
	}
	var one struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(b, &one) == nil && one.Name != "" {
		*p = []string{one.Name}
	}
	return nil
}

// variableObject is the history object whose payload names are variables.
const variableObject = "Variable"

type historyNode struct {
	ID         string       `json:"id"`
	Object     string       `json:"object"`
	Action     string       `json:"action"`
	CreatedAt  string       `json:"createdAt"`
	ServiceIDs []string     `json:"serviceIds"`
	Payload    payloadNames `json:"payload"`
	WorkflowID *string      `json:"workflowId"`
	Actor      *struct {
		Principal *string `json:"principal"`
	} `json:"actor"`
}

func (n historyNode) entry() HistoryEntry {
	at, _ := time.Parse(time.RFC3339Nano, n.CreatedAt)
	e := HistoryEntry{
		ID: n.ID, Object: n.Object, Action: n.Action, CreatedAt: at,
		ServiceIDs: n.ServiceIDs,
	}
	if n.Object == variableObject { // another object's name is not a variable's
		e.Names = n.Payload
	}
	if n.WorkflowID != nil {
		e.WorkflowID = *n.WorkflowID
	}
	if n.Actor != nil && n.Actor.Principal != nil {
		e.Actor = *n.Actor.Principal
	}
	return e
}

// EnvironmentHistory reads one page (100 entries) of an environment's
// history, every object type, from cursor after ("" = the first page).
// A non-zero since limits it to entries since then. Partial data comes with a
// *PartialError.
func (c *Client) EnvironmentHistory(ctx context.Context, envID, after string, since time.Time) (HistoryPage, error) {
	vars := map[string]any{"environmentId": envID, "after": nil, "since": nil}
	if after != "" {
		vars["after"] = after
	}
	if !since.IsZero() {
		vars["since"] = since.UTC().Format(time.RFC3339)
	}
	var d struct {
		History struct {
			PageInfo struct {
				HasNextPage bool    `json:"hasNextPage"`
				EndCursor   *string `json:"endCursor"`
			} `json:"pageInfo"`
			edges[historyNode]
		} `json:"environmentHistory"`
	}
	perr := c.doPartial(ctx, "EnvironmentHistory", environmentHistoryDoc, vars, &d, func() bool {
		return len(d.History.Edges) > 0
	})
	if perr != nil && !isPartial(perr) {
		return HistoryPage{}, perr
	}
	var p HistoryPage
	for _, n := range d.History.nodes() {
		p.Entries = append(p.Entries, n.entry())
	}
	if d.History.PageInfo.EndCursor != nil {
		p.EndCursor = *d.History.PageInfo.EndCursor
	}
	p.HasNext = d.History.PageInfo.HasNextPage && p.EndCursor != ""
	return p, perr
}

// maxPatchPages caps the pages EnvironmentPatches reads.
const maxPatchPages = 10

// EnvironmentPatches reads an environment's patches with their messages,
// newest first, those at or after since (zero: all), up to maxPatchPages
// pages of 100. Partial data comes with a *PartialError.
func (c *Client) EnvironmentPatches(ctx context.Context, envID string, since time.Time) ([]EnvPatch, error) {
	var out []EnvPatch
	var warnings []string
	var after any
	for range maxPatchPages {
		var d struct {
			Patches struct {
				PageInfo struct {
					HasNextPage bool    `json:"hasNextPage"`
					EndCursor   *string `json:"endCursor"`
				} `json:"pageInfo"`
				edges[struct {
					ID        string  `json:"id"`
					Message   *string `json:"message"`
					CreatedAt string  `json:"createdAt"`
				}]
			} `json:"environmentPatches"`
		}
		vars := map[string]any{"environmentId": envID, "after": after}
		perr := c.doPartial(ctx, "EnvironmentPatches", environmentPatchesDoc, vars, &d, func() bool {
			return len(d.Patches.Edges) > 0
		})
		var pe *PartialError
		if errors.As(perr, &pe) {
			warnings = append(warnings, pe.Warnings...)
		} else if perr != nil {
			return nil, perr
		}
		older := false
		for _, n := range d.Patches.nodes() {
			at, _ := time.Parse(time.RFC3339Nano, n.CreatedAt)
			if !since.IsZero() && at.Before(since) {
				older = true
				continue
			}
			p := EnvPatch{ID: n.ID, CreatedAt: at}
			if n.Message != nil {
				p.Message = *n.Message
			}
			out = append(out, p)
		}
		info := d.Patches.PageInfo
		if older || !info.HasNextPage || info.EndCursor == nil || *info.EndCursor == "" {
			break
		}
		after = *info.EndCursor
	}
	if len(warnings) > 0 {
		return out, &PartialError{Warnings: warnings}
	}
	return out, nil
}

// ProjectMembers reads the members of a project's workspace (Railway's
// projectMembers answers none for a workspace project); a project without a
// workspace has none. Partial data comes with a *PartialError.
func (c *Client) ProjectMembers(ctx context.Context, projectID string) ([]Member, error) {
	var d struct {
		Project *struct {
			Workspace *struct {
				Members []struct {
					ID    string  `json:"id"`
					Name  *string `json:"name"`
					Email string  `json:"email"`
				} `json:"members"`
			} `json:"workspace"`
		} `json:"project"`
	}
	perr := c.doPartial(ctx, "ProjectMembers", projectMembersDoc, map[string]any{"projectId": projectID}, &d, func() bool {
		return d.Project != nil && d.Project.Workspace != nil && len(d.Project.Workspace.Members) > 0
	})
	if perr != nil && !isPartial(perr) {
		return nil, perr
	}
	if d.Project == nil || d.Project.Workspace == nil {
		return nil, perr
	}
	var out []Member
	for _, m := range d.Project.Workspace.Members {
		mem := Member{ID: m.ID, Email: m.Email}
		if m.Name != nil {
			mem.Name = *m.Name
		}
		out = append(out, mem)
	}
	return out, perr
}
