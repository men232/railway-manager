package apply

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/workdir"
)

// Run states.
const (
	RunRunning   = "running"
	RunFailed    = "failed"
	RunAbandoned = "abandoned"
	RunFinished  = "finished"
)

// Flags Report gives a change kept in the live set.
const (
	FlagNotDeployed   = "written, not deployed"
	flagSkippedPrefix = "skipped: "
	flagFailedPrefix  = "failed: "
)

// RunInfo is one run as List shows it.
type RunInfo struct {
	ID, State string
	Created   time.Time
	Changes   int
	Steps     map[string]StepRecord
	// PatchIDs maps each patch id the journal's step records hold to the
	// environment of its step ("patch:<env>"); nil when there is none. A
	// step keeps only the patch id of its last attempt.
	PatchIDs map[string]string
	// Frozen is the run's frozen changes without their values, in order.
	Frozen []FrozenChange
	// Actions are the redeploy and restart actions frozen with the run;
	// nil when its actions.json is missing or unreadable.
	Actions []FrozenAction
	// Outcomes are the journal's change outcomes by change id.
	Outcomes map[string]Outcome
	// Elsewhere: the run is running on disk and another live process
	// holds its lock (workdir.HeldByOther: not a reused pid), so it
	// executes in another app instance; HolderPID is that process.
	Elsewhere bool
	HolderPID int
}

// FrozenChange is a change of a run's frozen set as List shows it: never a
// value. Fingerprint is Runs.Fingerprint of the change ("" when none).
type FrozenChange struct {
	ID, Kind, Group, Target string
	Place                   domain.Place
	Fingerprint             string
	// Overlay is the kind's overlay of the change ("→ 64 chars"), never a
	// value: from the change while it holds one, else as saved when its
	// values were scrubbed.
	Overlay Overlay
}

// RunError reports a run List or Unfinished skipped. Its text is fixed: it
// names the run and never quotes its files, which can hold values.
type RunError struct {
	RunID string
	What  string // "journal is corrupt", "frozen change set is unreadable", …
}

func (e *RunError) Error() string { return "run " + text.Sanitize(e.RunID) + ": " + e.What }

// Runs binds frozen change sets to journals under Dir/runs/<run id>/ and
// runs them through one shared executor. Runs of other app instances in the
// same work dir count as unfinished runs too: they are read from disk.
type Runs struct {
	Dir  string // work dir; runs live in Dir/runs
	Reg  Registry
	Exec *Executor
	// NewEnv makes the Env a run executes with; Execute sets its Journal,
	// RunID and RedeployOff.
	NewEnv func(runID string, j *Journal) *Env
	// Snapshot is the store copy Compile and Vet read (nil: none); Check
	// loads it once for both.
	Snapshot func() *store.Store
	// Compile compiles a change set against a store snapshot.
	Compile func(s *store.Store, cs changeset.ChangeSet) (Graph, error)
	// NewRedeploy builds a redeploy step, to restore one frozen in
	// actions.json that a later Compile no longer derives (the same
	// function Compile is given).
	NewRedeploy func(p domain.Place, serves, deps []string, wait bool) Step
	// Vet refuses a selection sel of the change set live before anything
	// is frozen: a database password group that cannot run. It gets the
	// snapshot Compile read and the compiled redeploy and restart actions;
	// when sel does not compile it is asked with nil actions, and its
	// refusal, naming the reason, wins over the compile error. Nil vets
	// nothing.
	Vet func(s *store.Store, sel, live changeset.ChangeSet, actions []FrozenAction) error
	// Fingerprint is the digest of the secret a frozen change sets, for
	// List's FrozenChange ("" when none, or when nil): compared with the
	// places' values, never shown. List calls it off the UI goroutine.
	Fingerprint func(changeset.Change) string
	// KeepAbandoned reports a change whose value an abandoned run keeps:
	// a database password a revert still logs in with. Every other value
	// leaves a run's files once it is finished or abandoned (scrub). Nil
	// keeps none.
	KeepAbandoned func(changeset.Change) bool

	// mu serialises Prepare and PrepareImmediate within the process;
	// Dir/prepare.lock does it across app instances.
	mu sync.Mutex
}

// prepareLockWait bounds how long Prepare waits for another instance's
// prepare lock.
const prepareLockWait = 5 * time.Second

// PrepareLock is the held prepare lock (LockPrepare): the in-process mutex
// and Dir/prepare.lock. Its Prepare or PrepareImmediate releases it, as does
// Release; it is used once.
type PrepareLock struct {
	r    *Runs
	l    *workdir.Lock
	once sync.Once
}

// LockPrepare takes the prepare lock, waiting up to prepareLockWait for
// another instance's. It blocks, so a UI calls it off its own goroutine and
// then prepares on the lock it gets.
func (r *Runs) LockPrepare() (*PrepareLock, error) {
	r.mu.Lock()
	l, err := r.lockPrepare()
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	return &PrepareLock{r: r, l: l}, nil
}

// Release releases the lock; later calls do nothing.
func (p *PrepareLock) Release() error {
	var err error
	p.once.Do(func() {
		err = p.l.Release()
		p.r.mu.Unlock()
	})
	return err
}

// lockPrepare takes Dir/prepare.lock, so no two app instances check
// conflicts and create a run at the same time. Another instance holds it
// only for that short moment, so a held lock is retried for a while.
func (r *Runs) lockPrepare() (*workdir.Lock, error) {
	if err := workdir.Ensure(r.runsDir()); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(prepareLockWait)
	for {
		l, err := workdir.AcquireLock(filepath.Join(r.Dir, "prepare.lock"))
		var le *workdir.LockedError
		if err == nil || !errors.As(err, &le) || time.Now().After(deadline) {
			return l, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Report is what a run did to its changes, by change id. A change is Done
// when every step serving it, and every step those added, is done (or
// skipped) and its own outcome is not skipped; it leaves the live set.
// Otherwise it is Kept with a flag: "skipped: <reason>", "written, not
// deployed" or "failed: <first error line>". State is the run state Execute
// set (finished or failed).
type Report struct {
	Done, Kept map[string]string
	State      string
}

func (r *Runs) runsDir() string { return filepath.Join(r.Dir, "runs") }

// runDir returns the directory of runID, refusing ids that are not a plain
// directory name.
func (r *Runs) runDir(runID string) (string, error) {
	if runID == "" || runID == "." || runID == ".." || strings.ContainsAny(runID, `/\`) {
		return "", fmt.Errorf("invalid run id %q", runID)
	}
	return filepath.Join(r.runsDir(), runID), nil
}

// loadFrozen reads a run's frozen change set. Unlike changeset.Load it never
// moves a file aside, and a missing file is an error.
func loadFrozen(dir string) (changeset.ChangeSet, error) {
	var cs changeset.ChangeSet
	data, err := os.ReadFile(filepath.Join(dir, "changeset.json"))
	if err != nil {
		return cs, err
	}
	// The decoder's error can quote the file's content, secret values
	// included: it is never shown.
	if json.Unmarshal(data, &cs) != nil {
		return cs, fmt.Errorf("run %s: frozen change set is %w", filepath.Base(dir), errCorrupt)
	}
	return cs, nil
}

// openRun opens the journal of an existing run.
func openRun(dir string) (*Journal, error) {
	path := filepath.Join(dir, "journal.json")
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("run %s not found", filepath.Base(dir))
		}
		return nil, err
	}
	return OpenJournal(path)
}

// ErrRunsUnreadable is the report of List and Unfinished when the runs
// directory itself cannot be read: no run is known then, and none may be
// assumed gone.
var ErrRunsUnreadable = errors.New("runs directory is unreadable")

// journals opens every runs/*/journal.json, whoever wrote it. A run whose
// journal cannot be read is skipped and reported as a *RunError.
func (r *Runs) journals() (map[string]*Journal, []error) {
	entries, err := os.ReadDir(r.runsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{ErrRunsUnreadable}
	}
	out := make(map[string]*Journal, len(entries))
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		path := filepath.Join(r.runsDir(), id, "journal.json")
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			continue // not a run, or one still being frozen
		}
		j, err := OpenJournal(path)
		if err != nil {
			errs = append(errs, &RunError{RunID: id, What: unreadable("journal", err)})
			continue
		}
		out[id] = j
	}
	return out, errs
}

// unreadable is the fixed text of a file that could not be used: corrupt
// when it does not decode, unreadable otherwise. Decoder errors can quote
// the file's content, so none is kept.
func unreadable(what string, err error) string {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	if errors.As(err, &se) || errors.As(err, &te) || errors.Is(err, errCorrupt) {
		return what + " is corrupt"
	}
	return what + " is unreadable"
}

// errCorrupt marks a frozen file that does not decode.
var errCorrupt = errors.New("corrupt")

// frozenOf is loadFrozen for List and Unfinished: a run it cannot read gives
// a *RunError.
func (r *Runs) frozenOf(id string) (changeset.ChangeSet, error) {
	cs, err := loadFrozen(filepath.Join(r.runsDir(), id))
	if err != nil {
		return cs, &RunError{RunID: id, What: unreadable("frozen change set", err)}
	}
	return cs, nil
}

// List returns every run in the work dir, newest first. A run it cannot read
// (corrupt journal or frozen change set) is skipped and reported in errs, so
// one bad run never hides the others.
func (r *Runs) List() ([]RunInfo, []error) {
	js, errs := r.journals()
	out := make([]RunInfo, 0, len(js))
	for id, j := range js {
		cs, err := r.frozenOf(id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		steps := j.snapshot()
		in := RunInfo{ID: id, State: j.State, Created: j.Created, Changes: len(cs.Changes), Steps: steps, PatchIDs: patchIDs(steps)}
		dir := filepath.Join(r.runsDir(), id)
		fps, _ := loadFingerprints(dir) // a scrubbed run's
		overlays, _ := loadOverlays(dir)
		in.Frozen = make([]FrozenChange, len(cs.Changes))
		for i, c := range cs.Changes {
			in.Frozen[i] = FrozenChange{ID: c.ID, Kind: c.Kind, Group: c.Group, Target: c.Target, Place: c.Place, Fingerprint: fps[c.ID], Overlay: overlays[c.ID]}
			if c.New != nil {
				if r.Fingerprint != nil {
					in.Frozen[i].Fingerprint = r.Fingerprint(c)
				}
				in.Frozen[i].Overlay = r.overlay(c)
			}
		}
		if actions, err := loadActions(dir); err == nil {
			in.Actions = actions
			if in.Actions == nil {
				in.Actions = []FrozenAction{}
			}
		}
		in.Outcomes = j.outcomes()
		if in.State == RunRunning {
			in.HolderPID, in.Elsewhere = workdir.HeldByOther(filepath.Join(dir, "lock"))
			if !in.Elsewhere {
				in.HolderPID = 0
			}
		}
		out = append(out, in)
	}
	slices.SortFunc(out, func(a, b RunInfo) int {
		return cmp.Or(b.Created.Compare(a.Created), cmp.Compare(b.ID, a.ID))
	})
	sortErrs(errs)
	return out, errs
}

// Frozen reads a run's frozen change set, values included: for the root's
// revert, which logs in with the password the run set. It reads the disk, so
// a UI calls it off its own goroutine.
func (r *Runs) Frozen(runID string) (changeset.ChangeSet, error) {
	dir, err := r.runDir(runID)
	if err != nil {
		return changeset.ChangeSet{}, err
	}
	return loadFrozen(dir)
}

// patchIDs maps the patch ids of steps to their environment, from the step
// key "<kind>:<env>" or "<kind>:<env>/<rest>".
func patchIDs(steps map[string]StepRecord) map[string]string {
	var out map[string]string
	for k, rec := range steps {
		if rec.PatchID == "" {
			continue
		}
		_, loc, _ := strings.Cut(k, ":")
		env, _, _ := strings.Cut(loc, "/")
		if out == nil {
			out = map[string]string{}
		}
		out[rec.PatchID] = env
	}
	return out
}

// sortErrs orders the skipped runs' reports by text, so they read the same
// on every call.
func sortErrs(errs []error) {
	slices.SortFunc(errs, func(a, b error) int { return cmp.Compare(a.Error(), b.Error()) })
}

// Unfinished returns the frozen change sets of the runs that are running or
// failed, by run id, including those of other app instances. A run it
// cannot read is skipped and reported in errs, as by List.
func (r *Runs) Unfinished() (map[string]changeset.ChangeSet, []error) {
	js, errs := r.journals()
	out := map[string]changeset.ChangeSet{}
	for id, j := range js {
		if j.State != RunRunning && j.State != RunFailed {
			continue
		}
		cs, err := r.frozenOf(id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out[id] = cs
	}
	sortErrs(errs)
	return out, errs
}

// Prepare freezes a selection of the live set into a new run, holding
// Dir/prepare.lock across the conflict check and the run's creation. Empty ids
// select every unlocked valid change (Reg.CheckData). A selected change held
// by a run is refused with changeset.ErrLocked, an invalid one with its
// reason. The selection is checked against every
// unfinished run and against the unselected rest (OtherRun ""); on a
// conflict nothing is written and the conflicts are returned. Otherwise the
// run is written to runs/<id>/ in state running and the selection is locked
// in live.
func (r *Runs) Prepare(live *changeset.ChangeSet, ids []string) (runID string, conflicts []changeset.Conflict, err error) {
	p, err := r.LockPrepare()
	if err != nil {
		return "", nil, err
	}
	return p.Prepare(live, ids)
}

// active is Unfinished for a conflict check. A corrupt run is skipped: it
// cannot be checked against, and it must not block every apply (List and
// Unfinished report it). An unreadable runs dir refuses: no run is known.
func (r *Runs) active() (map[string]changeset.ChangeSet, error) {
	active, errs := r.Unfinished()
	for _, err := range errs {
		if errors.Is(err, ErrRunsUnreadable) {
			return nil, err
		}
	}
	return active, nil
}

// Plan is a selection Check found free of conflicts and compiled: what
// Freeze writes as a run.
type Plan struct {
	IDs     []string            // the selected change ids, in live-set order
	Set     changeset.ChangeSet // the selected changes and the redeploy opt-outs
	Actions []FrozenAction
	Reopen  map[string][]string // the compiled Graph.Reopen
}

// Prepare is Runs.Prepare under the lock p holds; it releases p. It is
// Check on a copy of live, Freeze under a new run id, then live.Lock: for
// callers that own live on their goroutine. A UI checks and freezes in its
// commands instead (Check, Freeze).
func (p *PrepareLock) Prepare(live *changeset.ChangeSet, ids []string) (runID string, conflicts []changeset.Conflict, err error) {
	defer func() { err = errors.Join(err, p.Release()) }()
	plan, conflicts, err := p.Check(live.Clone(), ids)
	if err != nil || len(conflicts) > 0 {
		return "", conflicts, err
	}
	runID = NewRunID()
	if err := p.Freeze(plan, runID); err != nil {
		return "", nil, err
	}
	if err := live.Lock(plan.IDs, runID); err != nil {
		return "", nil, errors.Join(err, p.r.remove(runID))
	}
	return runID, nil, nil
}

// Check is the first half of Prepare under the lock p holds, on snapshot, a
// copy of the live set (changeset.ChangeSet.Clone) that it only reads. Empty
// ids select every unlocked valid change (Reg.CheckData). A selected change
// held by a run is refused with changeset.ErrLocked, an invalid one with its
// reason. The selection is checked against every unfinished run and against
// the unselected rest (OtherRun ""); on a conflict the conflicts are
// returned. Otherwise the selection is compiled into a Plan, which Vet may
// refuse. Nothing is written, and p stays held.
func (p *PrepareLock) Check(snapshot changeset.ChangeSet, ids []string) (Plan, []changeset.Conflict, error) {
	r := p.r
	live := &snapshot
	if len(ids) == 0 {
		invalid := 0
		invalidWrites := map[string]bool{}
		var valid []changeset.Change
		for _, c := range live.Changes {
			switch {
			case c.RunID != "":
			case r.Reg.CheckData(c) != nil:
				invalid++
				for _, w := range r.Reg.Writes(c) {
					invalidWrites[w] = true
				}
			default:
				ids = append(ids, c.ID)
				valid = append(valid, c)
			}
		}
		if len(ids) == 0 && invalid > 0 {
			return Plan{}, nil, errors.New("nothing to apply: every unlocked change is invalid")
		}
		if len(ids) == 0 {
			return Plan{}, nil, errors.New("no unlocked changes to apply")
		}
		// A valid change reading what an invalid one writes cannot go
		// without it; say which, not just that they conflict.
		for _, c := range valid {
			if slices.ContainsFunc(r.Reg.Reads(c), func(loc string) bool { return invalidWrites[loc] }) {
				return Plan{}, nil, fmt.Errorf("%s depends on an invalid change · x removes it", text.Sanitize(c.Target))
			}
		}
	}
	for _, id := range ids {
		c, ok := live.Get(id)
		if !ok {
			return Plan{}, nil, fmt.Errorf("change %s not found", id)
		}
		if c.RunID != "" {
			return Plan{}, nil, fmt.Errorf("%s: %w", c.Key(), changeset.ErrLocked)
		}
		if err := r.Reg.CheckData(c); err != nil {
			return Plan{}, nil, fmt.Errorf("%s is invalid: %w", c.Target, err)
		}
	}
	selected, rest := live.Select(ids)
	active, err := r.active()
	if err != nil {
		return Plan{}, nil, err
	}
	// A change a listed run holds is checked through that run's frozen set.
	rest.Changes = slices.DeleteFunc(rest.Changes, func(c changeset.Change) bool {
		_, ok := active[c.RunID]
		return c.RunID != "" && ok
	})
	active[""] = rest
	if conflicts := changeset.CheckConflicts(r.Reg, selected, active); len(conflicts) > 0 {
		return Plan{}, conflicts, nil
	}
	snap := r.snapshot()
	actions, reopen, err := r.compileActions(snap, selected)
	if verr := r.vet(snap, selected, *live, actions, err); verr != nil {
		return Plan{}, nil, verr
	}
	if err != nil {
		return Plan{}, nil, err
	}
	planIDs := make([]string, len(selected.Changes))
	for i, c := range selected.Changes {
		planIDs[i] = c.ID
	}
	return Plan{IDs: planIDs, Set: selected, Actions: actions, Reopen: reopen}, nil, nil
}

// Freeze is the second half of Prepare under the lock p holds: it writes plan
// as the new run runID (see freeze). A run id already taken gives an error
// wrapping fs.ErrExist and leaves that run alone. p stays held.
func (p *PrepareLock) Freeze(plan Plan, runID string) error {
	return p.r.freeze(runID, plan.Set, plan.Actions, plan.Reopen)
}

// PrepareImmediate is Prepare for a one-change set that is not in the live
// set, such as a restart from a key on a screen. An invalid change is
// refused with its reason, as by Prepare; a valid one is checked against the
// unfinished runs only.
func (r *Runs) PrepareImmediate(c changeset.Change) (runID string, conflicts []changeset.Conflict, err error) {
	p, err := r.LockPrepare()
	if err != nil {
		return "", nil, err
	}
	return p.PrepareImmediate(c)
}

// PrepareImmediate is Runs.PrepareImmediate under the lock p holds; it
// releases p.
func (p *PrepareLock) PrepareImmediate(c changeset.Change) (runID string, conflicts []changeset.Conflict, err error) {
	defer func() { err = errors.Join(err, p.Release()) }()
	r := p.r
	if c.ID == "" {
		c.ID = changeset.NewID()
	}
	if err := r.Reg.CheckData(c); err != nil {
		return "", nil, fmt.Errorf("%s is invalid: %w", c.Target, err)
	}
	cs := changeset.ChangeSet{Changes: []changeset.Change{c}}
	active, err := r.active()
	if err != nil {
		return "", nil, err
	}
	if conflicts := changeset.CheckConflicts(r.Reg, cs, active); len(conflicts) > 0 {
		return "", conflicts, nil
	}
	snap := r.snapshot()
	actions, reopen, err := r.compileActions(snap, cs)
	if verr := r.vet(snap, cs, cs, actions, err); verr != nil {
		return "", nil, verr
	}
	if err != nil {
		return "", nil, err
	}
	runID = NewRunID()
	if err := r.freeze(runID, cs, actions, reopen); err != nil {
		return "", nil, err
	}
	return runID, nil, nil
}

// NewRunID returns a new run id: the UTC time to the second and 12 random
// hex characters.
func NewRunID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("apply: crypto/rand failed: " + err.Error())
	}
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// freeze writes cs as the new run runID: runs/<id>/changeset.json (0600),
// the frozen actions in actions.json (0600), the compiled Graph.Reopen in
// reopen.json (0600, only when there is one) and a journal in state running.
// The run directory must not exist: a taken id gives an error wrapping
// fs.ErrExist and nothing is written or removed. On a later failure the
// directory is removed.
func (r *Runs) freeze(runID string, cs changeset.ChangeSet, actions []FrozenAction, reopen map[string][]string) error {
	dir, err := r.runDir(runID)
	if err != nil {
		return err
	}
	if err := workdir.Ensure(r.runsDir()); err != nil {
		return err
	}
	frozen := changeset.ChangeSet{RedeployOff: cs.RedeployOff}
	for _, c := range cs.Changes {
		c.RunID, c.Flag = "", ""
		frozen.Changes = append(frozen.Changes, c)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err // never remove a directory this call did not make
	}
	err = changeset.Save(filepath.Join(dir, "changeset.json"), frozen)
	if err == nil {
		err = saveActions(dir, actions)
	}
	if err == nil {
		err = saveReopen(dir, reopen)
	}
	if err == nil {
		var j *Journal
		if j, err = OpenJournal(filepath.Join(dir, "journal.json")); err == nil {
			err = j.SetState(RunRunning)
		}
	}
	if err != nil {
		return errors.Join(err, r.remove(runID))
	}
	return nil
}

func (r *Runs) remove(runID string) error {
	dir, err := r.runDir(runID)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// Execute runs (or resumes) a run under its lock file: a lock held by a
// live process gives *workdir.LockedError. It compiles the frozen change set,
// adds the frozen actions the compile no longer derives (restoreActions)
// and the frozen reopen lists (restoreReopen), runs the executor and sets
// the run state: finished when every step of the graph and every step this
// Execute added is done or skipped, failed otherwise. When ctx ends first,
// the state goes back to what it was before Execute, the Report is empty and
// ctx's error is returned: callers must not Reconcile then. Otherwise the
// Report is returned with the executor's error, if any.
func (r *Runs) Execute(ctx context.Context, runID string, onEvent func(Event)) (rep Report, err error) {
	dir, err := r.runDir(runID)
	if err != nil {
		return Report{}, err
	}
	if _, err := os.Stat(dir); err != nil {
		return Report{}, fmt.Errorf("run %s: %w", runID, err)
	}
	lock, err := workdir.AcquireLock(filepath.Join(dir, "lock"))
	if err != nil {
		return Report{}, err
	}
	defer func() { err = errors.Join(err, lock.Release()) }()

	j, err := openRun(dir)
	if err != nil {
		return Report{}, err
	}
	if j.State == RunFinished || j.State == RunAbandoned {
		return Report{}, fmt.Errorf("run %s is %s", runID, j.State)
	}
	cs, err := loadFrozen(dir)
	if err != nil {
		return Report{}, err
	}
	g, err := r.Compile(r.snapshot(), cs)
	var frozen []FrozenAction
	if err == nil {
		if frozen, err = loadActions(dir); err == nil {
			g, err = r.restoreActions(g, cs, frozen)
		}
	}
	if err == nil {
		var reopen map[string][]string
		if reopen, err = loadReopen(dir); err == nil {
			g = restoreReopen(g, reopen)
		}
	}
	if err != nil {
		return Report{}, errors.Join(fmt.Errorf("run %s: %w", runID, err), j.SetState(RunFailed))
	}
	prev := j.State
	if err := j.SetState(RunRunning); err != nil {
		return Report{}, err
	}

	env := r.NewEnv(runID, j)
	env.Journal, env.RunID = j, runID
	env.RedeployOff = make(map[string]bool, len(cs.RedeployOff))
	for _, k := range cs.RedeployOff {
		env.RedeployOff[k] = true
	}
	env.Changes = cs.Changes
	env.Actions = frozen
	tr := newTracker(g)
	runErr := r.Exec.Run(ctx, tr.graph, env, onEvent)
	if runErr != nil && ctx.Err() != nil && !errors.Is(runErr, ErrStopped) {
		return Report{}, errors.Join(runErr, j.SetState(prev))
	}

	state := RunFinished
	if runErr != nil || !tr.allFinal(j) {
		state = RunFailed
	}
	if err := j.SetState(state); err != nil {
		runErr = errors.Join(runErr, err)
	}
	if state == RunFinished {
		// Its values are in Railway now: none stays on disk.
		runErr = errors.Join(runErr, r.scrub(dir, cs, nil))
	}
	rep = report(cs, j, tr, runErr, r.Reg.Marker, state == RunFinished)
	rep.State = state
	return rep, runErr
}

// Abandon marks a run abandoned under its lock: it is not resumed and no
// longer counts as unfinished. What it already wrote stays. The caller then
// calls live.Unlock(runID, nil).
func (r *Runs) Abandon(runID string) (err error) {
	dir, err := r.runDir(runID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("run %s: %w", runID, err)
	}
	lock, err := workdir.AcquireLock(filepath.Join(dir, "lock"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Release()) }()
	j, err := openRun(dir)
	if err != nil {
		return err
	}
	if j.State == RunFinished {
		return fmt.Errorf("run %s is finished", runID)
	}
	if err := j.SetState(RunAbandoned); err != nil {
		return err
	}
	cs, err := loadFrozen(dir)
	if err != nil {
		return err
	}
	return r.scrub(dir, cs, r.KeepAbandoned)
}

// scrub rewrites a run's frozen change set without the values of its
// changes, keep's excepted, after saving their fingerprints (Fingerprint)
// for List: a run that will not act again needs no value, and one left on
// disk outlives every rotation of it.
func (r *Runs) scrub(dir string, cs changeset.ChangeSet, keep func(changeset.Change) bool) error {
	fps, _ := loadFingerprints(dir)
	overlays, _ := loadOverlays(dir)
	if overlays == nil {
		overlays = map[string]Overlay{}
	}
	scrubbed := false
	for i, c := range cs.Changes {
		if c.New == nil || (keep != nil && keep(c)) {
			continue
		}
		overlays[c.ID] = r.overlay(c)
		if r.Fingerprint != nil {
			if fp := r.Fingerprint(c); fp != "" {
				if fps == nil {
					fps = map[string]string{}
				}
				fps[c.ID] = fp
			}
		}
		cs.Changes[i].New, scrubbed = nil, true
	}
	if !scrubbed {
		return nil
	}
	if len(fps) > 0 {
		if err := saveFingerprints(dir, fps); err != nil {
			return err
		}
	}
	if err := saveOverlays(dir, overlays); err != nil {
		return err
	}
	return changeset.Save(filepath.Join(dir, "changeset.json"), cs)
}

// overlay is the kind's overlay of c, its long form dropped; zero for an
// unknown kind.
func (r *Runs) overlay(c changeset.Change) Overlay {
	k, ok := r.Reg[c.Kind]
	if !ok {
		return Overlay{}
	}
	o := k.Overlay(c)
	return Overlay{Short: o.Short, Tone: o.Tone}
}

// saveOverlays writes the overlays of a scrubbed run's changes, by change
// id: what each did, never a value.
func saveOverlays(dir string, overlays map[string]Overlay) error {
	data, err := json.Marshal(overlays)
	if err != nil {
		return err
	}
	return workdir.WriteFileAtomic(filepath.Join(dir, "overlays.json"), data, 0o600)
}

func loadOverlays(dir string) (map[string]Overlay, error) {
	data, err := os.ReadFile(filepath.Join(dir, "overlays.json"))
	if err != nil {
		return nil, err
	}
	var out map[string]Overlay
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// saveFingerprints writes the fingerprints of a scrubbed run's values, by
// change id: digests, never values.
func saveFingerprints(dir string, fps map[string]string) error {
	data, err := json.Marshal(fps)
	if err != nil {
		return err
	}
	return workdir.WriteFileAtomic(filepath.Join(dir, "fingerprints.json"), data, 0o600)
}

func loadFingerprints(dir string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "fingerprints.json"))
	if err != nil {
		return nil, err
	}
	var fps map[string]string
	if err := json.Unmarshal(data, &fps); err != nil {
		return nil, err
	}
	return fps, nil
}

// Prune removes the finished and abandoned runs last written more than age
// before now, with their journals: a failed or running run is kept, since
// it may resume. A run another instance holds is skipped. A younger one
// that still holds values is scrubbed (rescrub). It returns how many it
// removed.
func (r *Runs) Prune(age time.Duration, now time.Time) (int, []error) {
	js, errs := r.journals()
	n := 0
	for id, j := range js {
		if j.State != RunFinished && j.State != RunAbandoned {
			continue
		}
		dir := filepath.Join(r.runsDir(), id)
		info, err := os.Stat(filepath.Join(dir, "journal.json"))
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < age {
			if err := r.rescrub(dir, j.State); err != nil {
				errs = append(errs, &RunError{RunID: id, What: "cannot scrub: " + err.Error()})
			}
			continue
		}
		lock, err := workdir.AcquireLock(filepath.Join(dir, "lock"))
		if err != nil {
			continue // in use elsewhere
		}
		err = os.RemoveAll(dir)
		if rerr := lock.Release(); err == nil && rerr != nil && !os.IsNotExist(rerr) {
			err = rerr
		}
		if err != nil {
			errs = append(errs, &RunError{RunID: id, What: "cannot remove: " + err.Error()})
			continue
		}
		n++
	}
	return n, errs
}

// rescrub scrubs a finished or abandoned run that still holds values: it
// stopped between its final state and its scrub (a crash, a failed write).
// A run whose lock is held elsewhere is left for a later start.
func (r *Runs) rescrub(dir, state string) error {
	lock, err := workdir.AcquireLock(filepath.Join(dir, "lock"))
	if err != nil {
		return nil // in use elsewhere
	}
	defer func() { _ = lock.Release() }()
	cs, err := loadFrozen(dir)
	if err != nil {
		return err
	}
	var keep func(changeset.Change) bool
	if state == RunAbandoned {
		keep = r.KeepAbandoned
	}
	return r.scrub(dir, cs, keep)
}

// Reconcile applies a run's report to the live set: Done changes leave it.
// The run's other changes get their flags; they are unlocked only when the
// run finished. After a failed run they stay read-only until it
// is resumed to the end or abandoned (then the caller calls
// live.Unlock(runID, nil)).
func Reconcile(live *changeset.ChangeSet, runID string, rep Report) {
	live.Drop(slices.Collect(maps.Keys(rep.Done)))
	if rep.State == RunFinished {
		live.Unlock(runID, rep.Kept)
		return
	}
	live.Flag(runID, rep.Kept)
}

// tracker records which changes each step serves, including the steps
// added at run time through Result.More, which the graph does not hold.
type tracker struct {
	graph Graph
	order []string // graph order, then added keys as first seen

	mu     sync.Mutex
	serves map[string][]string // step key → change ids
}

func newTracker(g Graph) *tracker {
	t := &tracker{serves: map[string][]string{}}
	t.graph = Graph{Steps: make(map[string]Step, len(g.Steps)), Order: g.Order, Reopen: g.Reopen}
	for k, st := range g.Steps {
		t.graph.Steps[k] = trackedStep{Step: st, t: t}
	}
	for _, k := range g.Order {
		if st, ok := g.Steps[k]; ok {
			t.add(st)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(g.Steps)) {
		t.add(g.Steps[k])
	}
	return t
}

// add records st's changes. Steps sharing a key are one step to the
// executor, so their changes are joined.
func (t *tracker) add(st Step) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := st.Key()
	if _, ok := t.serves[k]; !ok {
		t.order = append(t.order, k)
		t.serves[k] = nil
	}
	for _, id := range st.Serves() {
		if !slices.Contains(t.serves[k], id) {
			t.serves[k] = append(t.serves[k], id)
		}
	}
}

// servers returns the keys of the steps serving change id, in order.
func (t *tracker) servers(id string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for _, k := range t.order {
		if slices.Contains(t.serves[k], id) {
			out = append(out, k)
		}
	}
	return out
}

// allFinal reports whether every step of the graph and every step added in
// this Execute is done or skipped in the journal. Steps an earlier Execute
// added and a later rebuild no longer returns are not judged.
func (t *tracker) allFinal(j *Journal) bool {
	t.mu.Lock()
	keys := slices.Clone(t.order)
	t.mu.Unlock()
	for _, k := range keys {
		if !final(j.Step(k).Status) {
			return false
		}
	}
	return true
}

// trackedStep records the steps its Run adds and tracks them in turn.
type trackedStep struct {
	Step
	t *tracker
}

func (s trackedStep) Unwrap() Step { return s.Step }

func (s trackedStep) Run(ctx context.Context, env *Env, prior StepRecord) (Result, error) {
	res, err := s.Step.Run(ctx, env, prior)
	if len(res.More) > 0 {
		more := make([]Step, len(res.More))
		for i, m := range res.More {
			s.t.add(m)
			more[i] = trackedStep{Step: m, t: s.t}
		}
		res.More = more
	}
	return res, err
}

func isAction(key string) bool {
	return strings.HasPrefix(key, redeployPrefix) || strings.HasPrefix(key, restartPrefix)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(s)
}

// leftovers returns the keys a step added in any Execute, recursively
// through the journal's Expanded, that are not done or skipped.
func leftovers(j *Journal, key string, seen map[string]bool) []string {
	var out []string
	for _, e := range j.Step(key).Expanded {
		if seen[e] {
			continue
		}
		seen[e] = true
		if !final(j.Step(e).Status) {
			out = append(out, e)
		}
		out = append(out, leftovers(j, e, seen)...)
	}
	return out
}

// report decides, per change of the frozen set, whether the run is done
// with it (see Report). A step a serving step added (journaled Expanded)
// and not finished counts as a deploy still left, whatever it serves.
//
// A marker change (marker true: a decision with no step) is done once the
// run finished, and kept until then.
func report(cs changeset.ChangeSet, j *Journal, t *tracker, runErr error, marker func(changeset.Change) bool, finished bool) Report {
	rep := Report{Done: map[string]string{}, Kept: map[string]string{}}
	for _, c := range cs.Changes {
		if o := j.Outcome(c.ID); o.Status == StatusSkipped {
			rep.Kept[c.ID] = flagSkippedPrefix + o.Reason
			continue
		}
		keys := t.servers(c.ID)
		if len(keys) == 0 && marker != nil && marker(c) {
			if finished {
				rep.Done[c.ID] = ""
			} else {
				rep.Kept[c.ID] = flagFailedPrefix + "run not finished"
			}
			continue
		}
		if len(keys) == 0 {
			if j.Outcome(c.ID).Status == StatusDone {
				rep.Done[c.ID] = ""
			} else {
				rep.Kept[c.ID] = flagFailedPrefix + "no step serves this change"
			}
			continue
		}
		seen := map[string]bool{}
		for _, k := range keys {
			seen[k] = true
		}
		var left []string
		for _, k := range keys {
			left = append(left, leftovers(j, k, seen)...)
		}
		writes, writesDone, actionLeft, blocked := 0, true, len(left) > 0, false
		var msg string
		for _, k := range slices.Concat(keys, left) {
			rec := j.Step(k)
			action := isAction(k)
			if !action && slices.Contains(keys, k) {
				writes++
			}
			if final(rec.Status) {
				continue
			}
			if action {
				actionLeft = true
			} else if slices.Contains(keys, k) {
				writesDone = false
			}
			if rec.Status == StatusBlocked {
				blocked = true
			}
			if msg == "" && rec.Error != "" {
				msg = rec.Error
			}
		}
		switch {
		case writesDone && !actionLeft:
			rep.Done[c.ID] = ""
		case writes > 0 && writesDone:
			rep.Kept[c.ID] = FlagNotDeployed
		case msg != "":
			rep.Kept[c.ID] = flagFailedPrefix + firstLine(msg)
		case blocked:
			rep.Kept[c.ID] = flagFailedPrefix + "blocked by a failed step"
		case runErr != nil:
			rep.Kept[c.ID] = flagFailedPrefix + firstLine(runErr.Error())
		default:
			rep.Kept[c.ID] = flagFailedPrefix + "not run"
		}
	}
	return rep
}

// FrozenAction is a redeploy or restart step as Compile made it when the run
// was prepared: its key, place, the change ids it serves, its deps and, for
// a redeploy, whether it waits for SUCCESS. A run keeps them in
// runs/<id>/actions.json, so a resume compiled against a store that no
// longer knows a reference still runs them. It holds no values.
type FrozenAction struct {
	Key    string       `json:"key"`
	Place  domain.Place `json:"place"`
	Serves []string     `json:"serves,omitempty"`
	Deps   []string     `json:"deps,omitempty"`
	Wait   bool         `json:"wait,omitempty"`
}

// snapshot is the store copy to compile and vet against; nil without one.
func (r *Runs) snapshot() *store.Store {
	if r.Snapshot == nil {
		return nil
	}
	return r.Snapshot()
}

// CompileNow compiles cs against the current snapshot.
func (r *Runs) CompileNow(cs changeset.ChangeSet) (Graph, error) {
	return r.Compile(r.snapshot(), cs)
}

// VetNow compiles sel against the current snapshot and asks Vet about it
// as part of live, as Prepare would, writing nothing: a caller checks a
// selection before an act it cannot take back. A compile error is
// returned when Vet has nothing to say.
func (r *Runs) VetNow(sel, live changeset.ChangeSet) error {
	snap := r.snapshot()
	actions, _, err := r.compileActions(snap, sel)
	if verr := r.vet(snap, sel, live, actions, err); verr != nil {
		return verr
	}
	return err
}

// vet asks Vet about sel (see Runs.Vet); compileErr is Compile's error, if
// any, which drops the actions.
func (r *Runs) vet(s *store.Store, sel, live changeset.ChangeSet, actions []FrozenAction, compileErr error) error {
	if r.Vet == nil {
		return nil
	}
	if compileErr != nil {
		actions = nil
	}
	return r.Vet(s, sel, live, actions)
}

// compileActions compiles cs and returns its redeploy and restart steps and
// its Graph.Reopen. Without a Compile there is nothing to freeze.
func (r *Runs) compileActions(s *store.Store, cs changeset.ChangeSet) ([]FrozenAction, map[string][]string, error) {
	if r.Compile == nil {
		return nil, nil, nil
	}
	g, err := r.Compile(s, cs)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[string]changeset.Change, len(cs.Changes))
	for _, c := range cs.Changes {
		byID[c.ID] = c
	}
	var out []FrozenAction
	for _, k := range g.Order {
		if !isAction(k) {
			continue
		}
		st := g.Steps[k]
		out = append(out, FrozenAction{
			Key: k, Place: actionPlace(k, st.Serves(), byID),
			Serves: slices.Clone(st.Serves()), Deps: slices.Clone(st.Deps()),
			Wait: strings.HasPrefix(k, redeployPrefix) && waits(st),
		})
	}
	return out, g.Reopen, nil
}

// actionPlace is the place of an action key ("redeploy:<env>/<service>"),
// with the project of a change it serves or, failing that, of any change in
// the same environment.
func actionPlace(key string, serves []string, byID map[string]changeset.Change) domain.Place {
	_, pk, _ := strings.Cut(key, ":")
	envID, svc, _ := strings.Cut(pk, "/")
	p := domain.Place{EnvID: envID, ServiceID: svc}
	for _, id := range serves {
		if c, ok := byID[id]; ok && c.Place.EnvID == envID {
			p.ProjectID = c.Place.ProjectID
			return p
		}
	}
	for _, id := range slices.Sorted(maps.Keys(byID)) {
		if c := byID[id]; c.Place.EnvID == envID {
			p.ProjectID = c.Place.ProjectID
			return p
		}
	}
	return p
}

func saveActions(dir string, actions []FrozenAction) error {
	if actions == nil {
		actions = []FrozenAction{}
	}
	data, err := json.Marshal(actions)
	if err != nil {
		return err
	}
	return workdir.WriteFileAtomic(filepath.Join(dir, "actions.json"), data, 0o600)
}

// loadActions reads a run's frozen actions; a run without the file (prepared
// without a Compile) has none.
func loadActions(dir string) ([]FrozenAction, error) {
	data, err := os.ReadFile(filepath.Join(dir, "actions.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []FrozenAction
	if json.Unmarshal(data, &out) != nil { // its error can quote the content
		return nil, errors.New("frozen actions are corrupt")
	}
	return out, nil
}

// saveReopen writes a run's reopen lists; nothing when there are none.
func saveReopen(dir string, reopen map[string][]string) error {
	if len(reopen) == 0 {
		return nil
	}
	data, err := json.Marshal(reopen)
	if err != nil {
		return err
	}
	return workdir.WriteFileAtomic(filepath.Join(dir, "reopen.json"), data, 0o600)
}

// loadReopen reads a run's frozen reopen lists; a run without the file has
// none.
func loadReopen(dir string) (map[string][]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "reopen.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out map[string][]string
	if json.Unmarshal(data, &out) != nil { // its error can quote the content
		return nil, errors.New("frozen reopen lists are corrupt")
	}
	return out, nil
}

// restoreReopen joins the frozen reopen lists into g's, so a resume reopens
// what the run was prepared to reopen even when a later compile no longer
// lists it. Keys of steps absent from g are dropped.
func restoreReopen(g Graph, frozen map[string][]string) Graph {
	if len(frozen) == 0 {
		return g
	}
	out := maps.Clone(g.Reopen)
	for k, ls := range frozen {
		if _, ok := g.Steps[k]; !ok {
			continue
		}
		merged := slices.DeleteFunc(union(out[k], ls), func(l string) bool {
			_, ok := g.Steps[l]
			return !ok || l == k
		})
		if len(merged) == 0 {
			continue
		}
		slices.Sort(merged)
		if out == nil {
			out = map[string][]string{}
		}
		out[k] = merged
	}
	g.Reopen = out
	return g
}

// restoreActions adds to g every frozen action it lacks, and the frozen
// serves, deps and wait flag a redeploy in g lacks, so a resume never drops a
// redeploy the run was prepared with (a change leaves only when the
// redeploys it caused are done). A restart Compile no longer collapses into
// a restored redeploy at its place is folded into it, as Compile does.
// Deps on steps absent from g are dropped; the order is recomputed.
func (r *Runs) restoreActions(g Graph, cs changeset.ChangeSet, frozen []FrozenAction) (Graph, error) {
	if len(frozen) == 0 {
		return g, nil
	}
	steps := maps.Clone(g.Steps)
	renamed := map[string]string{} // folded restart key → redeploy key
	changed := false
	for _, fa := range frozen {
		switch {
		case strings.HasPrefix(fa.Key, redeployPrefix):
			serves, deps := slices.Clone(fa.Serves), slices.Clone(fa.Deps)
			wait := fa.Wait
			cur, inGraph := steps[fa.Key]
			if inGraph {
				serves = union(cur.Serves(), serves)
				deps = union(cur.Deps(), deps)
				wait = wait || waits(cur)
			}
			restart := restartPrefix + strings.TrimPrefix(fa.Key, redeployPrefix)
			rs, fold := steps[restart]
			if fold {
				serves = union(serves, rs.Serves())
				deps = union(deps, rs.Deps())
				delete(steps, restart)
				renamed[restart] = fa.Key
			}
			if inGraph && !fold && len(serves) == len(cur.Serves()) && len(deps) == len(cur.Deps()) && wait == waits(cur) {
				continue
			}
			if r.NewRedeploy == nil {
				return Graph{}, fmt.Errorf("cannot restore the frozen step %s", fa.Key)
			}
			deps = slices.DeleteFunc(deps, func(d string) bool { return d == fa.Key || d == restart })
			steps[fa.Key] = r.NewRedeploy(fa.Place, serves, deps, wait)
			changed = true
		case strings.HasPrefix(fa.Key, restartPrefix):
			if _, ok := steps[fa.Key]; ok {
				continue
			}
			redeploy := redeployPrefix + strings.TrimPrefix(fa.Key, restartPrefix)
			if _, ok := steps[redeploy]; ok {
				continue // collapsed into the redeploy, as at Prepare
			}
			st, err := r.ownStep(cs, fa)
			if err != nil {
				return Graph{}, err
			}
			steps[fa.Key] = st
			changed = true
		}
	}
	if !changed {
		return g, nil
	}
	// Point deps at folded restarts to their redeploy, and drop deps on
	// absent steps.
	for k, st := range steps {
		var deps []string
		for _, d := range st.Deps() {
			if to, ok := renamed[d]; ok {
				d = to
			}
			if _, ok := steps[d]; ok && d != k {
				deps = append(deps, d)
			}
		}
		slices.Sort(deps)
		deps = slices.Compact(deps)
		declared := slices.Clone(st.Deps())
		slices.Sort(declared)
		if !slices.Equal(slices.Compact(declared), deps) {
			steps[k] = wired{Step: st, deps: deps}
		}
	}
	order, err := topoOrder(steps)
	if err != nil {
		return Graph{}, err
	}
	return Graph{Steps: steps, Order: order, Reopen: g.Reopen}, nil
}

// ownStep rebuilds a frozen restart from the kind of a change it serves.
func (r *Runs) ownStep(cs changeset.ChangeSet, fa FrozenAction) (Step, error) {
	for _, c := range cs.Changes {
		if !slices.Contains(fa.Serves, c.ID) {
			continue
		}
		k, ok := r.Reg[c.Kind]
		if !ok {
			continue
		}
		for _, st := range k.OwnSteps(c) {
			if st.Key() == fa.Key {
				return st, nil
			}
		}
	}
	return nil, fmt.Errorf("cannot restore the frozen step %s", fa.Key)
}

// union returns a with the elements of b it lacks appended.
func union(a, b []string) []string {
	out := slices.Clone(a)
	for _, x := range b {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}
