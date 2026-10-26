package view

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/text"
)

// StalledRun is a database step of a run stalled after it acted, for the
// banner on Home and the Change set. Label is the row's label as the Rotate
// list shows it ("<service> root", "<service> user <name>", "<service>
// KEYFILE"; "<service> <user>" when the store has no such row), sanitized.
// Redeploys and NotRedeployed count the redeploys frozen with the run that
// serve the group's changes — for a password the apps only (not the
// database's owner or members), for a KEYFILE its nodes — and those of them
// not done; both 0 when the run's actions are unknown. Elsewhere: the run
// executes in another live app instance (process HolderPID); it is shown, not
// continued from here. It holds no value.
type StalledRun struct {
	RunID, Group, Label      string
	KeyFile, Elsewhere       bool
	HolderPID                int
	Redeploys, NotRedeployed int
}

// Stalled lists the stalled database steps of the runs, newest run first:
// a run failed, or running on disk and not applying here (one executing in
// another live instance is listed with Elsewhere), whose database step
// acted (actedSuffixes). It reads the runs index (as the root read it off
// the UI goroutine), the live states and the store (labels, owners);
// memoized on both.
func (v *Views) Stalled(runs RotateRuns) []StalledRun {
	k := key(append(rev(v.Store, store.Catalog, store.Variables), v.runsKey(runs))...)
	return v.stalled.Get(k, func() []StalledRun {
		v.stalledBuilds++
		return v.stalledRuns(runs)
	})
}

func (v *Views) stalledRuns(runs RotateRuns) []StalledRun {
	var out []StalledRun
	for _, run := range runs.Runs {
		if !unfinished(run, runs.Live) {
			continue
		}
		for _, sfx := range actedSuffixes(run) {
			dbKey, user := splitSuffix(sfx)
			group := rotation.DBGroupKey(dbKey, user)
			sr := StalledRun{RunID: run.ID, Group: group, KeyFile: user == rotation.KeyFileUser}
			sr.Elsewhere = run.Elsewhere && runs.Live[run.ID] == ""
			if sr.Elsewhere {
				sr.HolderPID = run.HolderPID
			}
			owner, members, label := v.groupDB(run, group)
			sr.Label = label
			exclude := map[string]bool{}
			if !sr.KeyFile {
				exclude[owner.Key()] = true
				for _, m := range members {
					exclude[m.Key()] = true
				}
			}
			sr.Redeploys, sr.NotRedeployed, _ = redeployCounts(run, group, exclude)
			out = append(out, sr)
		}
	}
	return out
}

// groupDB is the database of a group's frozen password or KEYFILE change:
// its owner, its members and the row label (passwordLabel) when the store
// has the row; else the change's place and "<service> <user>". Sanitized.
func (v *Views) groupDB(run apply.RunInfo, group string) (owner domain.Place, members []domain.Place, label string) {
	_, user, _ := rotation.ParseDBGroupKey(group)
	for _, c := range run.Frozen {
		if c.Group == group && (c.Kind == kindDBPassword || c.Kind == kindDBKeyFile) {
			owner = c.Place
			break
		}
	}
	if owner.EnvID == "" {
		return owner, nil, "database " + text.Sanitize(user)
	}
	o := rotation.Options{EnvIDs: []string{owner.EnvID}, AllEnvIDs: []string{owner.EnvID}}
	if pr, ok := rotation.RowOf(v.Store, group, o); ok {
		return owner, pr.DB.Members, passwordLabel(v.Store, pr)
	}
	svc := text.Sanitize(v.Store.ServiceName(owner.ServiceID))
	if user == rotation.KeyFileUser {
		return owner, nil, svc + " KEYFILE"
	}
	return owner, nil, svc + " " + text.Sanitize(user)
}

// passwordLabel is a password row's label as the Rotate list shows it.
func passwordLabel(s *store.Store, pr rotation.PasswordRow) string {
	label := text.Sanitize(s.ServiceName(pr.DB.Owner.ServiceID))
	switch {
	case pr.KeyFile:
		return label + " KEYFILE"
	case pr.Root:
		return label + " root"
	}
	return label + " user " + text.Sanitize(pr.User)
}

// splitSuffix splits a database step suffix "<db key>:<user>" (database
// keys hold no ":").
func splitSuffix(sfx string) (dbKey, user string) {
	dbKey, user, _ = strings.Cut(sfx, ":")
	return dbKey, user
}

// unfinished reports whether a run is stalled-shaped: failed, or running on
// disk while not applying here.
func unfinished(run apply.RunInfo, live map[string]string) bool {
	if l := live[run.ID]; l != "" {
		return l == apply.RunFailed
	}
	return run.State == apply.RunFailed || run.State == apply.RunRunning
}

// actedSuffixes are the "<db key>:<user>" of the run's database steps that
// acted, sorted: a dbpw step Acted, a pre-flight Acted in a run without
// that dbpw step (a password changed by hand), a KEYFILE verify not done
// once a patch of an environment of the KEYFILE group's frozen changes may
// have written (groupPatched).
func actedSuffixes(run apply.RunInfo) []string {
	var out []string
	for k, rec := range run.Steps {
		switch {
		case strings.HasPrefix(k, phaseDBPW):
			if rec.Acted {
				out = append(out, strings.TrimPrefix(k, phaseDBPW))
			}
		case strings.HasPrefix(k, phasePreflight):
			sfx := strings.TrimPrefix(k, phasePreflight)
			if _, ok := run.Steps[phaseDBPW+sfx]; !ok && rec.Acted {
				out = append(out, sfx)
			}
		case strings.HasPrefix(k, phaseVerify):
			sfx := strings.TrimPrefix(k, phaseVerify)
			if _, user := splitSuffix(sfx); user == rotation.KeyFileUser && rec.Status != apply.StatusDone &&
				groupPatched(run, rotation.DBGroupKey(splitSuffix(sfx))) {
				out = append(out, sfx)
			}
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// groupPatched reports whether a patch of an environment of the group's
// frozen changes may have written (patchTouched): done, or committed by
// Railway before the step failed.
func groupPatched(run apply.RunInfo, group string) bool {
	ids := map[string]bool{}
	for _, c := range run.Frozen {
		if c.Group == group {
			ids[c.ID] = true
		}
	}
	for _, c := range run.Frozen {
		if c.Group == group && patchTouched(run.Steps[phasePatch+c.Place.EnvID], ids) {
			return true
		}
	}
	return false
}

// DBActed reports whether a run's database step may have changed a
// database password or KEYFILE (actedSuffixes): abandoning it can leave
// places on the old one.
func DBActed(run apply.RunInfo) bool { return len(actedSuffixes(run)) > 0 }

// redeployCounts counts the redeploys frozen with run that serve a change
// of group, places in exclude aside, and those of them whose step is not
// done; ok is false when the run's actions are unknown.
func redeployCounts(run apply.RunInfo, group string, exclude map[string]bool) (total, notDone int, ok bool) {
	if run.Actions == nil {
		return 0, 0, false
	}
	ids := map[string]bool{}
	for _, c := range run.Frozen {
		if c.Group == group {
			ids[c.ID] = true
		}
	}
	for _, a := range run.Actions {
		if !strings.HasPrefix(a.Key, redeployPrefix) || exclude[strings.TrimPrefix(a.Key, redeployPrefix)] ||
			!slices.ContainsFunc(a.Serves, func(id string) bool { return ids[id] }) {
			continue
		}
		total++
		if run.Steps[a.Key].Status != apply.StatusDone {
			notDone++
		}
	}
	return total, notDone, true
}

// patchTouched reports whether a patch step may have written something:
// done, started, Acted, holding a patch id (Railway committed a patch, even
// when the step then failed polling), or recording a committed change of
// ids in its Data (Data it cannot read counts as touched).
func patchTouched(rec apply.StepRecord, ids map[string]bool) bool {
	if rec.Status == apply.StatusDone || rec.Status == apply.StatusStarted || rec.Acted || rec.PatchID != "" {
		return true
	}
	var d struct {
		Committed []string `json:"committed"`
	}
	if len(rec.Data) == 0 {
		return false
	}
	if json.Unmarshal(rec.Data, &d) != nil {
		return true // unreadable: it may record a write (fail closed)
	}
	return slices.ContainsFunc(d.Committed, func(id string) bool { return ids[id] })
}
