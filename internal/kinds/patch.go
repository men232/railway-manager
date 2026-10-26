package kinds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/rotation"
)

const (
	patchPoll    = 2 * time.Second
	patchTimeout = 60 * time.Second
)

// patchStep is the env.patch step: every patchable change of one
// environment in one environmentPatchCommit. It decides nothing
// about a change itself; it asks the change's kind.
type patchStep struct {
	envID, projectID string
	changes          []changeset.Change
	reg              apply.Registry
}

// NewPatchStep builds the env.patch step of one environment.
func NewPatchStep(envID string, changes []changeset.Change, reg apply.Registry, projectID string) apply.Step {
	return &patchStep{envID: envID, projectID: projectID, changes: changes, reg: reg}
}

func (s *patchStep) Key() string    { return "patch:" + s.envID }
func (s *patchStep) Deps() []string { return nil }
func (s *patchStep) Lock() string   { return "" }
func (s *patchStep) Label() string  { return "patch " + s.envID }
func (s *patchStep) Serves() []string {
	ids := make([]string, len(s.changes))
	for i, c := range s.changes {
		ids[i] = c.ID
	}
	return ids
}

// label names a change without its value.
func label(c changeset.Change) string { return c.Target + " at " + c.Place.Key() }

func labels(cs []changeset.Change) string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = label(c)
	}
	return strings.Join(out, ", ")
}

// Confirmer is a kind that confirms a committed change itself, for a target
// Check alone cannot confirm (a sealed variable). Kinds without it are
// confirmed when Check reports DriftAlreadyNew.
type Confirmer interface {
	Confirm(ctx context.Context, env *apply.Env, c changeset.Change) (bool, error)
}

func (s *patchStep) kind(c changeset.Change) (apply.Kind, error) {
	k, ok := s.reg[c.Kind]
	if !ok {
		return nil, fmt.Errorf("unknown kind %s", c.Kind)
	}
	return k, nil
}

// skipReason is the kind's SkipReason for a drifted change, or the target
// changing when the kind gives none.
func (s *patchStep) skipReason(ctx context.Context, env *apply.Env, c changeset.Change) string {
	if sr, ok := s.reg[c.Kind].(apply.SkipReasoner); ok {
		if r := sr.SkipReason(ctx, env, c); r != "" {
			return r
		}
	}
	return "changed in Railway since the change was made"
}

func (s *patchStep) confirm(ctx context.Context, env *apply.Env, c changeset.Change) (bool, error) {
	k, err := s.kind(c)
	if err != nil {
		return false, err
	}
	if cf, ok := k.(Confirmer); ok {
		return cf.Confirm(ctx, env, c)
	}
	d, err := k.Check(ctx, env, c)
	return d == apply.DriftAlreadyNew, err
}

// Run checks, commits, waits for the commit, confirms and adds the
// redeploys of places that reference a written variable by now.
//
// A prior attempt that committed a patch (prior.PatchID) is waited on first,
// so a retry never sends a second patch while the first still applies. When
// that patch did not fail it may have written. A drifted change stops the
// run when a patch of this step that did not fail carried it
// (the committed ids in the step's Data), or, after such a patch, when no
// earlier attempt journaled it skipped; otherwise it is skipped — but a
// write of a database password group is never skipped: an automatic URL
// rewrite is re-made on the URL Railway holds now (rebaseRewrite), and any
// other drifted one fails the step before anything is sent (a skip would
// leave its app on the old password while the run finishes).
//
// A run with prior.Status done is a rebuild: it neither checks, commits nor
// confirms, and only re-derives the redeploys from the outcomes journaled
// by the attempt that finished.
func (s *patchStep) Run(ctx context.Context, env *apply.Env, prior apply.StepRecord) (apply.Result, error) {
	if prior.Status == apply.StatusDone {
		return s.rebuild(ctx, env)
	}
	res := apply.Result{Outcomes: map[string]apply.Outcome{}}

	acted := false
	data := readPatchData(prior.Data)
	if prior.PatchID != "" {
		failed, err := pollPatch(ctx, env, prior.PatchID)
		if err != nil {
			return res, fmt.Errorf("earlier patch %s: %w", prior.PatchID, err)
		}
		acted = failed == ""
		if !acted && len(data.Last) > 0 {
			// The earlier patch wrote nothing: what only it carried was
			// never committed.
			data.Committed = slices.DeleteFunc(data.Committed, func(id string) bool { return slices.Contains(data.Last, id) })
			data.Last = nil
			if err := s.saveData(env, data); err != nil {
				return res, err
			}
		}
	}

	// (1)–(3) Check before acting.
	var todo, drifted []changeset.Change
	for _, c := range s.changes {
		k, err := s.kind(c)
		if err != nil {
			return res, err
		}
		d, err := k.Check(ctx, env, c)
		if err != nil {
			return res, fmt.Errorf("check %s: %w", label(c), err)
		}
		switch d {
		case apply.DriftAlreadyNew:
			res.Outcomes[c.ID] = apply.Outcome{Status: "done"}
		case apply.DriftChanged:
			drifted = append(drifted, c)
		default:
			todo = append(todo, c)
		}
	}
	// A drifted automatic rewrite of a database password group follows the
	// URL Railway holds now (rebaseRewrite); re-made, it is checked again.
	var rebased []changeset.Change
	drifted = slices.DeleteFunc(drifted, func(c changeset.Change) bool {
		if !dbGroupWrite(c) {
			return false
		}
		r, ok, err := rebaseRewrite(ctx, env, c)
		if err == nil && ok {
			rebased = append(rebased, r)
		}
		return err == nil && ok
	})
	for _, r := range rebased {
		d, err := (variableKind{}).Check(ctx, env, r)
		switch {
		case err != nil:
			return res, fmt.Errorf("check %s: %w", label(r), err)
		case d == apply.DriftAlreadyNew:
			res.Outcomes[r.ID] = apply.Outcome{Status: "done"}
		case d == apply.DriftNone && !slices.Contains(data.Committed, r.ID):
			todo = append(todo, r)
		default:
			drifted = append(drifted, r) // changed again after a patch carried it
		}
	}
	var refused []changeset.Change
	if len(drifted) > 0 {
		// A change a committed patch of this step carried may have been
		// written. Otherwise, after an earlier patch that did not fail, a
		// change is touched unless an earlier attempt skipped it as drifted
		// (journals from before the committed list) or failed it as a
		// database password write that drifted (never sent).
		var touched []changeset.Change
		for _, c := range drifted {
			prev := ""
			if env.Journal != nil {
				prev = env.Journal.Outcome(c.ID).Status
			}
			if slices.Contains(data.Committed, c.ID) || acted && prev != apply.StatusSkipped && !(dbGroupWrite(c) && prev == apply.StatusFailed) {
				touched = append(touched, c)
			}
		}
		if len(touched) > 0 {
			// The earlier patch may have written these: a human looks first.
			return res, &StopRunError{Msg: "changed in Railway after this apply already started on it: " + labels(touched)}
		}
		for _, c := range drifted {
			if dbGroupWrite(c) {
				// Skipped, the app would keep the old database password
				// while the run reads finished: the step fails instead.
				refused = append(refused, c)
				res.Outcomes[c.ID] = apply.Outcome{Status: apply.StatusFailed, Reason: driftNote(env, c)}
				continue
			}
			res.Outcomes[c.ID] = apply.Outcome{Status: "skipped", Reason: s.skipReason(ctx, env, c)}
		}
	}
	if len(refused) > 0 {
		// Nothing of this environment is sent: the patch fails as a whole,
		// as one Railway refused, and its redeploys stay blocked.
		notes := make([]string, len(refused))
		for i, c := range refused {
			notes[i] = driftNote(env, c)
		}
		return res, errors.New(strings.Join(notes, "; "))
	}

	// (4)–(7) Commit, wait and confirm what is left.
	if len(todo) > 0 {
		if err := s.commit(ctx, env, prior, data, todo, res.Outcomes); err != nil {
			return res, err
		}
	}

	// (8) Places that reference a written variable by now. This runs on a
	// retry too, when everything is already there: an earlier attempt may
	// have committed and failed before it got here.
	more, err := s.freshRedeploys(ctx, env, res.Outcomes)
	if err != nil {
		return res, err
	}
	res.More = more
	return res, nil
}

// commit sends one patch for todo, records its id and the ids it carries
// (added to data), waits for it and confirms each change, marking it done in
// outcomes.
func (s *patchStep) commit(ctx context.Context, env *apply.Env, prior apply.StepRecord, data patchData, todo []changeset.Change, outcomes map[string]apply.Outcome) error {
	patch := map[string]any{}
	for _, c := range todo {
		k, err := s.kind(c)
		if err != nil {
			return err
		}
		if frag, ok := k.Patch(c); ok {
			merge(patch, frag)
		}
	}
	msg := fmt.Sprintf("railway-manager %s: %d changes", env.RunID, len(todo))
	id, err := env.API.CommitPatch(ctx, s.envID, patch, msg)
	if err != nil {
		return err
	}
	data.Last = nil
	for _, c := range todo {
		if !slices.Contains(data.Committed, c.ID) {
			data.Committed = append(data.Committed, c.ID)
			data.Last = append(data.Last, c.ID)
		}
	}
	if env.Journal != nil {
		rec := env.Journal.Step(s.Key()) // the current record, not prior: keep what this attempt wrote
		rec.Status, rec.Error, rec.PatchID, rec.At = "started", "", id, nowOf(env)
		rec.Data = data.marshal()
		if err := env.Journal.Set(s.Key(), rec); err != nil {
			return err
		}
	}

	failed, err := pollPatch(ctx, env, id)
	if err != nil {
		return err
	}
	if failed != "" {
		// Nothing was written: only this patch's own ids leave the list.
		data.Committed = slices.DeleteFunc(data.Committed, func(id string) bool { return slices.Contains(data.Last, id) })
		data.Last = nil
		return errors.Join(fmt.Errorf("patch %s failed: %s", id, failed), s.saveData(env, data))
	}

	for _, c := range todo {
		env.Forget(c.Place)
	}
	var missing []changeset.Change
	for _, c := range todo {
		ok, err := s.confirm(ctx, env, c)
		if err != nil {
			return fmt.Errorf("confirm %s: %w", label(c), err)
		}
		if ok {
			outcomes[c.ID] = apply.Outcome{Status: "done"}
		} else {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return errors.New("not in Railway after commit: " + labels(missing))
	}
	return nil
}

// patchData is the patch step's StepRecord.Data: the ids of the changes a
// patch of this step carried that did not fail, across attempts (Committed),
// and those the latest patch added to that list (Last), so they leave it
// when that patch turns out failed. Ids only, never values.
type patchData struct {
	Committed []string `json:"committed,omitempty"`
	Last      []string `json:"last,omitempty"`
}

// readPatchData decodes a record's Data; anything else reads as empty.
func readPatchData(raw json.RawMessage) patchData {
	var d patchData
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &d)
	}
	return d
}

func (d patchData) marshal() json.RawMessage {
	if len(d.Committed) == 0 && len(d.Last) == 0 {
		return nil
	}
	raw, _ := json.Marshal(d)
	return raw
}

// saveData writes d into the step's current journal record.
func (s *patchStep) saveData(env *apply.Env, d patchData) error {
	if env.Journal == nil {
		return nil
	}
	rec := env.Journal.Step(s.Key())
	rec.Data = d.marshal()
	return env.Journal.Set(s.Key(), rec)
}

func nowOf(env *apply.Env) time.Time {
	if env.Now != nil {
		return env.Now()
	}
	return time.Now().UTC()
}

// pollPatch polls the patch status every 2 s for up to 60 s. It returns
// Railway's error for a FAILED patch, "" for a COMMITTED one.
func pollPatch(ctx context.Context, env *apply.Env, id string) (failed string, err error) {
	for waited := time.Duration(0); ; waited += patchPoll {
		status, lastErr, err := env.API.PatchStatus(ctx, id)
		if err != nil {
			return "", err
		}
		switch status {
		case "COMMITTED":
			return "", nil
		case "FAILED":
			if lastErr == "" {
				lastErr = "no error given"
			}
			return lastErr, nil
		}
		if waited >= patchTimeout {
			return "", errors.New("patch not committed after 60s")
		}
		if err := env.Sleep(ctx, patchPoll); err != nil {
			return "", err
		}
	}
}

// rebuild returns the redeploys of a done patch step from its journaled
// outcomes, changing nothing.
func (s *patchStep) rebuild(ctx context.Context, env *apply.Env) (apply.Result, error) {
	outcomes := map[string]apply.Outcome{}
	if env.Journal != nil {
		for _, c := range s.changes {
			outcomes[c.ID] = env.Journal.Outcome(c.ID)
		}
	}
	more, err := s.freshRedeploys(ctx, env, outcomes)
	return apply.Result{More: more}, err
}

// freshRedeploys re-reads the environment and returns a redeploy step for
// every service place that now references a variable a done change wrote,
// except places in env.RedeployOff and database places for their own
// password (dbOwnPassword). Places already in the graph come back
// too; the executor dedupes by key.
func (s *patchStep) freshRedeploys(ctx context.Context, env *apply.Env, outcomes map[string]apply.Outcome) ([]apply.Step, error) {
	var done []changeset.Change
	for _, c := range s.changes {
		if outcomes[c.ID].Status == "done" {
			done = append(done, c)
		}
	}
	written := writtenLocations(s.reg, done)
	if len(written) == 0 {
		return nil, nil
	}
	recs, err := env.API.DiscoverEnv(ctx, s.projectID, s.envID)
	if err != nil {
		return nil, fmt.Errorf("re-read %s after the patch: %w", s.envID, err)
	}
	var names serviceNamer
	if env.Store != nil {
		names = env.Store
	}
	refs := freshReferrers(recs, names, written)
	members := dbMembers(recs, names, done)
	svcs := make([]string, 0, len(refs))
	for svc := range refs {
		svcs = append(svcs, svc)
	}
	slices.Sort(svcs)
	var out []apply.Step
	for _, svc := range svcs {
		var serves []string
		wait := false // a database password's redeploy waits for SUCCESS
		for _, c := range done {
			if dbOwnPassword(c) && (svc == c.Place.ServiceID || members[c.ID][svc]) {
				continue // a database is not redeployed for its own password
			}
			if refs[svc][c.ID] {
				serves = append(serves, c.ID)
				wait = wait || strings.HasPrefix(c.Group, apply.DBGroupPrefix)
			}
		}
		if len(serves) == 0 {
			continue
		}
		p := domain.Place{ProjectID: s.projectID, EnvID: s.envID, ServiceID: svc}
		if env.Store != nil {
			p = env.Store.PlaceOf(s.envID, svc)
		}
		if env.RedeployOff[p.Key()] {
			continue // switched off by the user: stays off
		}
		out = append(out, NewRedeployStep(p, serves, []string{s.Key()}, wait))
	}
	return out, nil
}

// dbMembers maps each change setting a database's own password
// (dbOwnPassword) to the services of its members in recs: Mongo places
// whose variable of the same name references the owner's directly and
// that carry memberVars (rotation's database detection). Named references
// resolve only when names is not nil.
func dbMembers(recs []railway.VarRecord, names serviceNamer, done []changeset.Change) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	has := map[string]map[string]bool{} // service → variable names
	for _, r := range recs {
		if has[r.ServiceID] == nil {
			has[r.ServiceID] = map[string]bool{}
		}
		has[r.ServiceID][r.Name] = true
	}
	for _, c := range done {
		if !dbOwnPassword(c) || c.Target != rotation.RootVar("mongo") {
			continue
		}
		for _, r := range recs {
			if r.Name != c.Target || r.ServiceID == "" || r.ServiceID == c.Place.ServiceID || r.EnvID != c.Place.EnvID ||
				!has[r.ServiceID][memberVars[0]] || !has[r.ServiceID][memberVars[1]] {
				continue
			}
			for _, ref := range domain.ParseReferences(r.Unrendered) {
				if ref.Name != c.Target || ref.Shared || ref.Service == "" || names == nil ||
					names.ServiceIDByName(r.EnvID, ref.Service) != c.Place.ServiceID {
					continue
				}
				if out[c.ID] == nil {
					out[c.ID] = map[string]bool{}
				}
				out[c.ID][r.ServiceID] = true
			}
		}
	}
	return out
}

// merge deep-merges src into dst; nested objects merge, anything else from
// src replaces.
func merge(dst, src map[string]any) {
	for k, v := range src {
		sm, sok := v.(map[string]any)
		dm, dok := dst[k].(map[string]any)
		if sok && dok {
			merge(dm, sm)
			continue
		}
		if sok {
			cp := map[string]any{}
			merge(cp, sm)
			dst[k] = cp
			continue
		}
		dst[k] = v
	}
}
