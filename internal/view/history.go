package view

import (
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
)

// FeedRow is one history entry of the History feed. It never holds a value:
// Names are the payload's variable names, the only thing read from it. Its
// strings are raw; screens sanitize them at render.
//
// Who is the actor's member name, "?" without an actor. Env is the
// environment label ("project/env"), EnvName the environment's own name,
// and Service the entry's services by name, joined with
// ", " ("" without any). RunID is the railway-manager run that applied an
// EnvironmentPatch entry (Open decision 5), "" on every other object: Local
// when the run is in the local runs index, so it can be opened.
type FeedRow struct {
	At                                time.Time
	Who, Action, Object, Env, Service string
	EnvName                           string
	Names                             []string
	RunID                             string
	Local                             bool
}

// patchMessagePrefix starts the message of every patch railway-manager
// commits: "railway-manager <runId>: N changes".
const patchMessagePrefix = "railway-manager "

// Feed merges the history read so far of the environments, newest first
// (ties keep envIDs order). runs is the local runs index; an entry is
// recognised as ours by its patch id in a run's PatchIDs, else by the
// patch's message, which also names runs no longer held locally; a message
// makes the row the local run only when Me made the patch.
func (v *Views) Feed(envIDs []string, runs []apply.RunInfo) []FeedRow {
	k := key(append(rev(v.Store, store.Catalog, store.History, store.Members, store.Patches),
		strings.Join(envIDs, ","), runsDigest(runs), v.Me)...)
	return v.feed.Get(k, func() []FeedRow {
		v.feedBuilds++
		return v.feedRows(envIDs, runs)
	})
}

// runsDigest is a cache key part: the run ids and their patch ids.
func runsDigest(runs []apply.RunInfo) string {
	var b strings.Builder
	for _, r := range runs {
		b.WriteString(r.ID)
		b.WriteByte('\x01')
		ids := make([]string, 0, len(r.PatchIDs))
		for id := range r.PatchIDs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			b.WriteByte('|')
			b.WriteString(id)
		}
		b.WriteByte(';')
	}
	return b.String()
}

func (v *Views) feedRows(envIDs []string, runs []apply.RunInfo) []FeedRow {
	local := map[string]bool{}
	byPatch := map[string]runPatch{}
	for _, r := range runs {
		local[r.ID] = true
		for id, env := range r.PatchIDs {
			byPatch[id] = runPatch{run: r.ID, env: env}
		}
	}
	var out []FeedRow
	for _, env := range envIDs {
		entries, _, _, _ := v.Store.History(env)
		projectID := v.Store.PlaceOf(env, "").ProjectID
		label, name := v.Store.EnvLabel(env), v.Store.EnvName(env)
		for _, e := range entries {
			r := FeedRow{
				At: e.CreatedAt, Action: e.Action, Object: e.Object, Env: label, EnvName: name,
				Who: ByNone, Names: slices.Clone(e.Names),
			}
			if e.Actor != "" {
				r.Who = v.Store.MemberName(projectID, e.Actor)
			}
			svcs := make([]string, len(e.ServiceIDs))
			for i, id := range e.ServiceIDs {
				svcs[i] = v.Store.ServiceName(id)
			}
			r.Service = strings.Join(svcs, ", ")
			r.RunID, r.Local = v.runOf(e, byPatch, local)
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// runPatch is the run and environment of a patch id in the runs index.
type runPatch struct{ run, env string }

// runOf names the run that applied a patch entry. A patch id missing from
// the runs index is not proof of a foreign patch (the run may be removed,
// or the id an earlier attempt's): the patch's message decides then. A
// message is free text any project member can write, so it makes the entry
// the local run only for a patch Me made.
func (v *Views) runOf(e railway.HistoryEntry, byPatch map[string]runPatch, local map[string]bool) (string, bool) {
	if e.Object != "EnvironmentPatch" {
		return "", false
	}
	env, patchID, ok := parseWorkflow(e.WorkflowID)
	if !ok {
		return "", false
	}
	if rp, ok := byPatch[patchID]; ok && rp.env == env {
		return rp.run, true
	}
	msg, ok := v.Store.PatchMessage(env, patchID)
	if !ok {
		return "", false
	}
	id, ok := runIDOf(msg)
	if !ok {
		return "", false
	}
	return id, local[id] && v.Me != "" && e.Actor == v.Me
}

// parseWorkflow splits "commitChanges/<envId>/<patchId>".
func parseWorkflow(w string) (env, patchID string, ok bool) {
	parts := strings.Split(w, "/")
	if len(parts) != 3 || parts[0] != "commitChanges" || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// runIDOf reads the run id from "railway-manager <runId>: N changes".
func runIDOf(msg string) (string, bool) {
	rest, ok := strings.CutPrefix(msg, patchMessagePrefix)
	if !ok {
		return "", false
	}
	id, _, ok := strings.Cut(rest, ":")
	if !ok || id == "" || strings.ContainsAny(id, " \t\n/\\") {
		return "", false
	}
	return id, true
}
