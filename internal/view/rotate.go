package view

import (
	"encoding/json"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/store"
)

// RotatePlace is one line of a rotation row: a location (one variable name
// at one place) and its decision. It never holds a value: Pending is the
// change at that place and name, whoever made it, with its values cleared.
//
// Letter indexes the location's group in the row (0 = a), -1 for a sealed
// location or an added variable. ReadOnly: the environment is not a write
// environment. DB: the place belongs to a database.
// Drifted is the kind's store drift reason for Pending, "" when none.
//
// On a password row Letter is -1 and Own marks the
// password's own line: the database's own variable or the KEYFILE. Every
// other line is a URL place (or a literal copy) with its URLMode, ViaProxy,
// Unclear and Copy from rotation.URLPlace; a URLOther line is another
// user's, a URLOtherDB line another database's, shown only. Auto: Pending is
// the decision's automatic rewrite (or own value); Kept: Pending is a
// variable.keep. A Pending that is neither is the user's decision, or an edit
// made elsewhere when its group is not the row's.
type RotatePlace struct {
	Loc      rotation.Loc
	Letter   int
	Pending  *changeset.Change
	Overlay  apply.Overlay
	ReadOnly bool
	DB       bool
	Drifted  string

	Own                     bool
	URLMode                 rotation.URLMode
	ViaProxy, Unclear, Copy bool
	Auto, Kept              bool
}

// RotateRow is a rotation row with its decisions. Places has a line per
// location; Decidable counts the lines that can take a
// decision, Decided those of them with a change (never more than
// Decidable; a change on another line still shows as its Pending). Lines
// that cannot take one are read only or on a database place. Envs are the
// distinct environment labels of the places in the store's order. Refs are
// the variables that receive a location's value through references.
//
// A password row has Password set and no Groups. Its
// Places are every place in every environment read: the own line, the URL
// places (unrecognised ones too), the literal copies, then other users'
// URLs and other databases' same values (display only). Its Decidable
// counts the password itself, every rewritten place and copy and every
// unrecognised place (k is allowed read only); Decided the password change
// pending (Change) and the rewritten places with a change of the row's group
// or a keep; both are 0 for a row with a Problem. Blocked is why the row
// cannot run as it stands (first reason, sanitized; "" when it can, has a
// Problem or is Stalled). Run is the newest run (not abandoned) that acted on
// the database (PasswordRun). Stalled: it is failed, or running on disk and
// neither applying here nor executing in another instance (for a KEYFILE: and
// not verified) — the row then takes only continue or revert (the screen
// enforces it). Changed, when not Stalled: the run was created after the
// leak time, or, without one, it finished; a revert run never is. Revert: a
// Stalled root password whose stalled run's new password (its frozen
// db.password's fingerprint) no place of the row holds, and whose run wrote
// no environment of the row's changes — revert database password is offered
// only then. Envs count the own line, rewritten and following places only.
type RotateRow struct {
	rotation.Row
	Places             []RotatePlace
	Decided, Decidable int
	Envs               []string
	Refs               []RefChain

	Password                 *rotation.PasswordRow
	Change                   *changeset.Change
	Blocked                  string
	Run                      *PasswordRun
	Stalled, Changed, Revert bool
}

// PasswordRun is a run that acted on a password row's database (see
// passwordRun). State is the live state when this process executes it, else
// the journal's; Applying: it executes in this process; Elsewhere: it is
// running in another live app instance. Verified: its dbverify step is
// done. Redeploys counts the redeploys frozen with the run that serve the
// row's changes (without frozen actions: its redeploy steps at the row's
// places), NotRedeployed those of them not done. Revert: the run sets the
// database back to the password in Railway (its frozen db.password of the
// row has no fingerprint: Runs.Fingerprint gives none for a revert only); it
// never marks the row Changed. It holds no value.
type PasswordRun struct {
	ID, State                             string
	Created                               time.Time
	Applying, Elsewhere, Verified, Revert bool
	Redeploys, NotRedeployed              int
}

// RotateRuns is what Rotate reads of the runs: the runs index (newest
// first, as the root last read it off the UI goroutine) and Live, the
// state of each run this process executes by id (tui.Ctx.Running), which
// wins over the index's: running there means applying.
type RotateRuns struct {
	Runs []apply.RunInfo
	Live map[string]string
}

// RefChain is a variable that references one of a row's locations, directly
// or through other references. Label is the chain from it back to the
// location: "VAR ← svc.VAR ← …". DB: the variable sits on a database
// place.
type RefChain struct {
	Loc, Label string
	DB         bool
}

// Rotate lists the rotation rows of the options' environments joined with
// the change set: the store's GEN, ASK and hidden groups as rotation.Rows
// merges them, a row per added variable (pending variable.set changes in a
// rotation.AddGroupKey group) in SectionAdded, and a password row per
// rotation.PasswordRows in SectionDatabase (passwordRow), which replace
// the DB and URL rows of value rotation. A place is read only unless its
// environment is in writeEnvs (none when empty). runs is what the password
// rows read of the runs, for their run state.
func (v *Views) Rotate(cs *changeset.ChangeSet, o rotation.Options, writeEnvs []string, runs RotateRuns) []RotateRow {
	k := key(append(rev(v.Store, store.Catalog, store.Variables),
		csRev(cs), optionsKey(o), strings.Join(writeEnvs, ","), v.inventoriesRead(changes(cs)), v.runsKey(runs))...)
	return v.rotate.Get(k, func() []RotateRow {
		v.rotateBuilds++
		return v.rotateRows(changes(cs), o, writeEnvs, runs)
	})
}

func optionsKey(o rotation.Options) string {
	since := ""
	if !o.Since.IsZero() {
		since = o.Since.UTC().Format(time.RFC3339Nano)
	}
	return strings.Join(o.EnvIDs, ",") + "|" + strings.Join(o.AllEnvIDs, ",") + "|" + strings.Join(o.Internal, ",") + "|" + since
}

func (v *Views) rotateRows(cl []changeset.Change, o rotation.Options, writeEnvs []string, runs RotateRuns) []RotateRow {
	a := rotation.Analyze(v.Store, o)
	byPlace := variableChanges(cl)
	envOrder := map[string]int{}
	for _, p := range v.Store.Projects() {
		for _, e := range p.Environments {
			envOrder[e.ID] = len(envOrder)
		}
	}
	line := func(loc rotation.Loc, letter int) RotatePlace {
		rp := RotatePlace{
			Loc: loc, Letter: letter,
			ReadOnly: !slices.Contains(writeEnvs, loc.Place.EnvID),
			DB:       a.IsDBPlace(loc.Place),
		}
		if c := byPlace[loc.Place.Key()][loc.Name]; c != nil {
			rp.Pending = stripValues(c)
			rp.Overlay = v.changeOverlay(c)
			rp.Drifted = v.drift(*c)
		}
		return rp
	}
	envLabels := func(places []RotatePlace) []string {
		var envIDs []string
		for _, p := range places {
			if !slices.Contains(envIDs, p.Loc.Place.EnvID) {
				envIDs = append(envIDs, p.Loc.Place.EnvID)
			}
		}
		sort.SliceStable(envIDs, func(i, j int) bool { return envOrder[envIDs[i]] < envOrder[envIDs[j]] })
		out := make([]string, len(envIDs))
		for i, id := range envIDs {
			out[i] = v.Store.EnvLabel(id)
		}
		return out
	}
	finish := func(r rotation.Row, places []RotatePlace) RotateRow {
		rr := RotateRow{Row: r, Places: places}
		for _, p := range places {
			if !p.ReadOnly && !p.DB {
				rr.Decidable++
				if p.Pending != nil {
					rr.Decided++
				}
			}
		}
		rr.Envs = envLabels(places)
		rr.Refs = v.refChains(a, places)
		return rr
	}

	var out []RotateRow
	for _, r := range rotation.Rows(a) {
		if r.Section == rotation.SectionDatabase {
			continue // replaced by the password rows
		}
		var places []RotatePlace
		for gi, g := range r.Groups {
			for _, l := range g.Locs {
				letter := gi
				if l.Sealed {
					letter = -1
				}
				places = append(places, line(l, letter))
			}
		}
		out = append(out, finish(r, places))
	}

	added := addedRows(cl)
	at := sort.Search(len(out), func(i int) bool { return out[i].Section > rotation.SectionAdded })
	rows := make([]RotateRow, 0, len(added))
	for _, ad := range added {
		places := make([]RotatePlace, len(ad.changes))
		for i, c := range ad.changes {
			places[i] = line(rotation.Loc{
				ID: domain.LocationID(c.Place.EnvID, c.Place.ServiceID, c.Target), Name: c.Target, Place: c.Place,
			}, -1)
		}
		rows = append(rows, finish(rotation.Row{
			Key: ad.group, Label: ad.name, Class: rotation.ClassAsk, Section: rotation.SectionAdded,
		}, places))
	}
	out = slices.Insert(out, at, rows...)

	// The database section comes first: its rows go before every other.
	var dbRows []RotateRow
	for _, pr := range rotation.PasswordRows(v.Store, a, o) {
		rr := v.passwordRow(pr, cl, line, writeEnvs, runs, o.Since)
		var shown []RotatePlace
		for _, p := range rr.Places {
			if p.Own || p.URLMode == rotation.URLRewrite || p.URLMode == rotation.URLFollows {
				shown = append(shown, p)
			}
		}
		rr.Envs = envLabels(shown)
		dbRows = append(dbRows, rr)
	}
	return append(dbRows, out...)
}

// changeOverlay is the kind's overlay of a change; zero for an unknown kind.
func (v *Views) changeOverlay(c *changeset.Change) apply.Overlay {
	if c == nil {
		return apply.Overlay{}
	}
	if k, ok := v.Reg[c.Kind]; ok {
		return k.Overlay(*c)
	}
	return apply.Overlay{}
}

type addedRow struct {
	group, name string
	changes     []*changeset.Change
}

// addedRows collects the pending variable.set changes of each
// rotation.AddGroupKey group, sorted by name.
func addedRows(cl []changeset.Change) []addedRow {
	byGroup := map[string]*addedRow{}
	var out []*addedRow
	for i := range cl {
		c := &cl[i]
		if c.Kind != "variable.set" || c.Group != rotation.AddGroupKey(c.Target) {
			continue
		}
		ad := byGroup[c.Group]
		if ad == nil {
			ad = &addedRow{group: c.Group, name: c.Target}
			byGroup[c.Group] = ad
			out = append(out, ad)
		}
		ad.changes = append(ad.changes, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	rows := make([]addedRow, len(out))
	for i, ad := range out {
		rows[i] = *ad
	}
	return rows
}

// refChains finds the variables that receive the places' values through
// references, each with its shortest chain back to the place, sorted by
// label. A variable reached from several places keeps its first chain.
func (v *Views) refChains(a rotation.Analysis, places []RotatePlace) []RefChain {
	own := map[string]bool{}
	for _, p := range places {
		own[p.Loc.ID] = true
	}
	seen := map[string]bool{}
	var out []RefChain
	for _, p := range places {
		for _, rc := range v.chainsFrom(a, p.Loc.ID) {
			if own[rc.Loc] || seen[rc.Loc] {
				continue
			}
			seen[rc.Loc] = true
			out = append(out, rc)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Label != out[j].Label {
			return out[i].Label < out[j].Label
		}
		return out[i].Loc < out[j].Loc
	})
	return out
}

// chainsFrom walks the dependents of root breadth first, keeping each one's
// parent, so every dependent gets its shortest chain.
func (v *Views) chainsFrom(a rotation.Analysis, root string) []RefChain {
	deps := v.Store.Dependents(root)
	if len(deps) == 0 {
		return nil
	}
	targets := map[string]map[string]bool{} // dependent → locations it references
	for _, d := range deps {
		sv := v.Store.Variable(d)
		if sv == nil {
			continue
		}
		t := map[string]bool{}
		for _, r := range domain.ParseReferences(sv.Unrendered) {
			if id := v.Store.ResolveReference(sv.Place, r); id != "" {
				t[id] = true
			}
		}
		targets[d] = t
	}
	parent := map[string]string{}
	visited := map[string]bool{root: true}
	var order []string
	queue := []string{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, d := range deps {
			if !visited[d] && targets[d][cur] {
				visited[d] = true
				parent[d] = cur
				order = append(order, d)
				queue = append(queue, d)
			}
		}
	}
	out := make([]RefChain, 0, len(order))
	for _, d := range order {
		sv := v.Store.Variable(d)
		parts := []string{sv.Name}
		for cur := parent[d]; ; cur = parent[cur] {
			parts = append(parts, v.qualified(cur))
			if cur == root {
				break
			}
		}
		out = append(out, RefChain{Loc: d, Label: strings.Join(parts, " ← "), DB: a.IsDBPlace(sv.Place)})
	}
	return out
}

// qualified is "svc.VAR" for a location ("shared.VAR" in the shared scope).
func (v *Views) qualified(locID string) string {
	sv := v.Store.Variable(locID)
	if sv == nil {
		return locID
	}
	if sv.Place.Shared() {
		return "shared." + sv.Name
	}
	return v.Store.ServiceName(sv.Place.ServiceID) + "." + sv.Name
}

// Change kinds a password row reads; this package does not import the kinds.
const (
	kindDBPassword = "db.password"
	kindDBKeyFile  = "db.keyfile"
	kindKeep       = "variable.keep"
)

// Step key prefixes of a password's steps ("<phase>:<db key>:<user>") and
// of a redeploy ("redeploy:<place key>").
const (
	phasePreflight = "preflight:"
	phaseDBPW      = "dbpw:"
	phaseVerify    = "dbverify:"
	phasePatch     = "patch:"
)

// runsKey is the memo key part of the runs: what passwordRun reads. The
// index part is computed once per runs index read (the root replaces the
// slice on every read) and keeps only runs with a database step; the live
// states are appended on every call.
func (v *Views) runsKey(r RotateRuns) string {
	var first *apply.RunInfo
	if len(r.Runs) > 0 {
		first = &r.Runs[0]
	}
	if first != v.runsKeyFirst || len(r.Runs) != v.runsKeyLen || first == nil {
		v.runsKeyFirst, v.runsKeyLen, v.runsKeyIndex = first, len(r.Runs), indexKey(r.Runs)
	}
	ids := slices.Sorted(maps.Keys(r.Live))
	var b strings.Builder
	b.WriteString(v.runsKeyIndex)
	for _, id := range ids {
		b.WriteString("|live:" + id + "=" + r.Live[id])
	}
	return b.String()
}

// indexKey keys the runs with a database step by id, created time, state,
// Elsewhere and the status and Acted of their database, patch and redeploy
// steps (a patch's patch id presence and Data too). Frozen changes and
// actions never change for a run id.
func indexKey(runs []apply.RunInfo) string {
	var b strings.Builder
	for _, run := range runs {
		keys := make([]string, 0, len(run.Steps))
		db := false
		for k := range run.Steps {
			switch {
			case strings.HasPrefix(k, phasePreflight), strings.HasPrefix(k, phaseDBPW), strings.HasPrefix(k, phaseVerify):
				db = true
				keys = append(keys, k)
			case strings.HasPrefix(k, redeployPrefix), strings.HasPrefix(k, phasePatch):
				keys = append(keys, k)
			}
		}
		if !db {
			continue
		}
		b.WriteString(run.ID + "|" + run.Created.UTC().Format(time.RFC3339Nano) + "|" + run.State)
		if run.Elsewhere {
			b.WriteString("|elsewhere:" + strconv.Itoa(run.HolderPID))
		}
		sort.Strings(keys)
		for _, k := range keys {
			rec := run.Steps[k]
			b.WriteString("|" + k + "=" + rec.Status)
			if rec.Acted {
				b.WriteByte('!')
			}
			if strings.HasPrefix(k, phasePatch) { // what revertable reads
				if rec.PatchID != "" {
					b.WriteByte('#')
				}
				b.Write(rec.Data) // change ids only
			}
		}
		b.WriteByte(';')
	}
	return b.String()
}

// passwordRow joins a password row with the change set and the runs.
func (v *Views) passwordRow(pr rotation.PasswordRow, cl []changeset.Change, line func(rotation.Loc, int) RotatePlace, writeEnvs []string, runs RotateRuns, since time.Time) RotateRow {
	r := rotation.Row{Key: pr.Key, Label: passwordLabel(v.Store, pr), Class: rotation.ClassDB, Section: rotation.SectionDatabase, Warning: pr.Warning, Problem: pr.Problem}
	if !pr.KeyFile && !pr.Root {
		r.Class = rotation.ClassURL
	}
	rr := RotateRow{Row: r, Password: &pr}

	for i := range cl {
		if c := &cl[i]; c.Group == pr.Key && (c.Kind == kindDBPassword || c.Kind == kindDBKeyFile) {
			rr.Change = stripValues(c)
		}
	}
	if pr.Own != nil {
		p := line(*pr.Own, -1)
		p.Own = true
		markDecision(&p, pr.Key)
		rr.Places = append(rr.Places, p)
	}
	for _, u := range slices.Concat(pr.URLs, pr.Copies, pr.Others) {
		p := line(u.Loc, -1)
		p.URLMode, p.ViaProxy, p.Unclear, p.Copy = u.Mode, u.ViaProxy, u.Unclear, u.Copy
		markDecision(&p, pr.Key)
		rr.Places = append(rr.Places, p)
	}

	if pr.Problem == "" {
		rr.Decidable = 1
		if rr.Change != nil {
			rr.Decided = 1
		}
		for _, p := range rr.Places {
			if p.Own || p.URLMode != rotation.URLRewrite && p.URLMode != rotation.URLUnrecognised {
				continue
			}
			rr.Decidable++
			if p.Pending != nil && p.Pending.Group == pr.Key {
				rr.Decided++
			}
		}
	}

	var info *apply.RunInfo
	rr.Run, info = v.passwordRun(pr, rr.Places, runs)
	if run := rr.Run; run != nil {
		unfinished := run.State == apply.RunFailed || run.State == apply.RunRunning && !run.Applying && !run.Elsewhere
		rr.Stalled = unfinished && (!pr.KeyFile || !run.Verified)
		rr.Revert = rr.Stalled && pr.Root && !pr.KeyFile && v.revertable(pr, rr.Places, *info)
		if !rr.Stalled && !run.Revert {
			if since.IsZero() {
				rr.Changed = run.State == apply.RunFinished
			} else {
				rr.Changed = run.Created.After(since)
			}
		}
	}
	if !rr.Stalled && pr.Problem == "" {
		rr.Blocked = v.passwordBlocked(pr, rr, cl, writeEnvs)
	}
	return rr
}

// markDecision sets Auto or Kept from a place's pending change of the
// row's group; another group's change (a keep too) is foreign.
func markDecision(p *RotatePlace, group string) {
	c := p.Pending
	switch {
	case c == nil || c.Group != group:
	case c.Kind == kindKeep:
		p.Kept = true
	case rotation.Automatic(*c):
		p.Auto = true
	}
}

// passwordBlocked is why a password row cannot run as it stands: the
// database's environments (rotation.DBRefusal), a user row whose URLs hold
// different passwords while one of them is not decided by hand, then the
// first refusing
// place (rotation.PlaceRefusal, shared with the password decision), in the
// places' order. "" when none. Sanitized.
func (v *Views) passwordBlocked(pr rotation.PasswordRow, rr RotateRow, cl []changeset.Change, writeEnvs []string) string {
	write := func(env string) bool { return slices.Contains(writeEnvs, env) }
	if msg := rotation.DBRefusal(v.Store, pr, write); msg != "" {
		return msg
	}
	if revertPending(pr.Key, cl) {
		// A revert sets the database back to what its places hold: none
		// needs a decision (kinds.CheckPasswordGroups).
		return ""
	}
	if !pr.Root && !pr.KeyFile {
		if _, mixed := rotation.CurrentPassword(v.Store, pr); mixed {
			for _, p := range rr.Places {
				if !p.Own && p.URLMode == rotation.URLRewrite && (p.Pending == nil || p.Auto || p.Pending.Group != pr.Key) {
					return "the user's URLs hold different passwords · decide each place"
				}
			}
		}
	}
	for _, p := range rr.Places {
		u := rotation.URLPlace{Loc: p.Loc, Mode: p.URLMode, ViaProxy: p.ViaProxy, Unclear: p.Unclear, Copy: p.Copy}
		if msg := rotation.PlaceRefusal(v.Store, pr, u, cl, write, rr.Change != nil); msg != "" {
			return msg
		}
	}
	return ""
}

// revertPending reports whether the pending password change of group is a
// revert (its source, read from New; the value is never kept).
func revertPending(group string, cl []changeset.Change) bool {
	for _, c := range cl {
		if c.Group != group || c.Kind != kindDBPassword {
			continue
		}
		var nv struct {
			Source string `json:"source"`
		}
		if json.Unmarshal(c.New, &nv) == nil && nv.Source == "revert" {
			return true
		}
	}
	return false
}

// passwordRun finds the newest run, abandoned ones aside, whose journal
// shows it acted on the row's database: a password's dbpw step Acted, or
// its pre-flight Acted when the run has no dbpw (a password changed by
// hand); a KEYFILE's run (its dbverify step) once a patch of the owner's
// or a member's environment may have written (patchTouched: done, or
// committed by Railway before the step failed).
func (v *Views) passwordRun(pr rotation.PasswordRow, places []RotatePlace, runs RotateRuns) (*PasswordRun, *apply.RunInfo) {
	suffix := pr.DB.Owner.Key() + ":" + pr.User
	// A password counts the apps it redeploys, not the database's owner
	// and members; a KEYFILE counts its nodes (owner and members).
	nodes := map[string]bool{pr.DB.Owner.Key(): true}
	patches := map[string]bool{phasePatch + pr.DB.Owner.EnvID: true}
	for _, m := range pr.DB.Members {
		nodes[m.Key()] = true
		if pr.KeyFile {
			patches[phasePatch+m.EnvID] = true
		}
	}
	targets := map[string]bool{}
	exclude := map[string]bool{}
	if pr.KeyFile {
		for k := range nodes {
			targets[redeployPrefix+k] = true
		}
	} else {
		exclude = nodes
		for _, p := range places {
			if (p.Own || p.URLMode == rotation.URLRewrite || p.URLMode == rotation.URLFollows) && !nodes[p.Loc.Place.Key()] {
				targets[redeployPrefix+p.Loc.Place.Key()] = true
			}
		}
	}
	acted := func(run apply.RunInfo) bool {
		if pr.KeyFile {
			if _, ok := run.Steps[phaseVerify+suffix]; !ok {
				return false
			}
			ids := map[string]bool{}
			for _, c := range run.Frozen {
				if c.Group == pr.Key {
					ids[c.ID] = true
				}
			}
			for k, rec := range run.Steps {
				if patches[k] && patchTouched(rec, ids) {
					return true
				}
			}
			return false
		}
		rec, ok := run.Steps[phaseDBPW+suffix]
		if !ok {
			rec, ok = run.Steps[phasePreflight+suffix]
		}
		return ok && rec.Acted
	}
	for _, run := range runs.Runs { // newest first
		state := run.State
		if live := runs.Live[run.ID]; live != "" {
			state = live
		}
		if state == apply.RunAbandoned || !acted(run) {
			continue
		}
		out := &PasswordRun{
			ID: run.ID, State: state, Created: run.Created,
			Applying:  runs.Live[run.ID] == apply.RunRunning,
			Elsewhere: run.Elsewhere && state == apply.RunRunning && runs.Live[run.ID] == "",
			Verified:  run.Steps[phaseVerify+suffix].Status == apply.StatusDone,
			Revert: slices.ContainsFunc(run.Frozen, func(c apply.FrozenChange) bool {
				return c.Group == pr.Key && c.Kind == kindDBPassword && c.Fingerprint == ""
			}),
		}
		var ok bool
		if out.Redeploys, out.NotRedeployed, ok = redeployCounts(run, pr.Key, exclude); !ok {
			for k, st := range run.Steps {
				if targets[k] {
					out.Redeploys++
					if st.Status != apply.StatusDone {
						out.NotRedeployed++
					}
				}
			}
		}
		return out, &run
	}
	return nil, nil
}

// revertable reports whether no place of a password row holds the new
// password of its stalled run: the run froze a
// fingerprint of it (Runs.Fingerprint; a revert has none), no readable
// place's value carries it, no place is sealed (it could hold it unseen),
// and no patch of an environment of the run's changes of the row may have
// written (patchTouched: the store can lag behind a write, and a patch can
// commit and then fail).
func (v *Views) revertable(pr rotation.PasswordRow, places []RotatePlace, run apply.RunInfo) bool {
	hashes := map[string]bool{}
	envs := map[string]bool{}
	for _, c := range run.Frozen {
		if c.Group != pr.Key {
			continue
		}
		if c.Kind == kindDBPassword && c.Fingerprint != "" {
			hashes[c.Fingerprint] = true
		}
		if c.Kind != kindKeep {
			envs[c.Place.EnvID] = true
		}
	}
	if len(hashes) == 0 {
		return false
	}
	ids := map[string]bool{}
	for _, c := range run.Frozen {
		if c.Group == pr.Key {
			ids[c.ID] = true
		}
	}
	for env := range envs {
		if patchTouched(run.Steps[phasePatch+env], ids) {
			return false
		}
	}
	for _, p := range places {
		sv := v.Store.Variable(p.Loc.ID)
		switch {
		case sv == nil:
		case sv.Sealed:
			return false
		case sv.Literal && rotation.HoldsPassword(sv.Unrendered, hashes):
			return false
		}
	}
	return true
}
