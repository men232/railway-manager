package kinds

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/text"
)

const (
	kindRedeploy = "service.redeploy"
	kindRestart  = "service.restart"

	busyPoll    = 15 * time.Second
	busyTimeout = 10 * time.Minute

	// A password group's redeploy waits for its new deployment: polled every
	// 10 s for up to 10 minutes. A deployment created up to deploySkew before
	// the call still counts as new, for a clock behind Railway's.
	deployPoll    = 10 * time.Second
	deployTimeout = 10 * time.Minute
	deploySkew    = time.Minute
)

var (
	busyRE       = regexp.MustCompile(`(?i)cannot redeploy yet`)
	inProgressRE = regexp.MustCompile(`^(QUEUED|WAITING|INITIALIZING|BUILDING|DEPLOYING)$`)
	// deadRE: a new deployment that will not reach SUCCESS by itself.
	deadRE = regexp.MustCompile(`^(FAILED|CRASHED|REMOVED|REMOVING|SKIPPED)$`)
)

// Redeploy makes an explicit service.redeploy change.
func Redeploy(p domain.Place, now time.Time) changeset.Change {
	return changeset.Change{ID: changeset.NewID(), Kind: kindRedeploy, Place: p, Target: "redeploy", Created: now}
}

// Restart makes a service.restart change.
func Restart(p domain.Place, now time.Time) changeset.Change {
	return changeset.Change{ID: changeset.NewID(), Kind: kindRestart, Place: p, Target: "restart", Created: now}
}

// actionKind is the part redeploy and restart share: no patch, no writes
// (the executor serialises actions per service), no drift.
type actionKind struct{}

func (actionKind) Validate(_ *store.Store, c changeset.Change) error {
	if c.Place.Shared() {
		return errors.New("the shared scope has no service to redeploy or restart")
	}
	return nil
}
func (actionKind) IsNoop(changeset.Change) bool                  { return false }
func (actionKind) Secret(changeset.Change) bool                  { return false }
func (actionKind) Writes(changeset.Change) []string              { return nil }
func (actionKind) Reads(changeset.Change) []string               { return nil }
func (actionKind) Patch(changeset.Change) (map[string]any, bool) { return nil, false }
func (actionKind) Redeploys(*store.Store, changeset.Change) []domain.Place {
	return nil
}
func (actionKind) Check(context.Context, *apply.Env, changeset.Change) (apply.Drift, error) {
	return apply.DriftNone, nil
}

type redeployKind struct{ actionKind }

func (redeployKind) Name() string { return kindRedeploy }
func (redeployKind) Overlay(changeset.Change) apply.Overlay {
	return apply.Overlay{Short: "↻ redeploy", Long: "↻ redeploy", Tone: "warn"}
}
func (redeployKind) OwnSteps(c changeset.Change) []apply.Step {
	return []apply.Step{NewRedeployStep(c.Place, []string{c.ID}, nil, false)}
}

type restartKind struct{ actionKind }

func (restartKind) Name() string { return kindRestart }
func (restartKind) Overlay(changeset.Change) apply.Overlay {
	return apply.Overlay{Short: "↻ restart", Long: "↻ restart", Tone: "warn"}
}
func (restartKind) OwnSteps(c changeset.Change) []apply.Step {
	return []apply.Step{NewRestartStep(c.Place, []string{c.ID})}
}

// actionStep is the shared shape of the redeploy and restart steps.
type actionStep struct {
	prefix       string
	place        domain.Place
	serves, deps []string
}

func (s *actionStep) Key() string      { return s.prefix + ":" + s.place.Key() }
func (s *actionStep) Deps() []string   { return s.deps }
func (s *actionStep) Serves() []string { return s.serves }
func (s *actionStep) Lock() string     { return s.place.Key() }
func (s *actionStep) Label() string    { return s.prefix + " " + s.place.Key() }

// skipped reports whether the run skipped the change (journaled outcome).
func skipped(env *apply.Env, id string) bool {
	return env.Journal != nil && env.Journal.Outcome(id).Status == apply.StatusSkipped
}

// idle reports whether every change the step serves was skipped, so it has
// nothing to deploy: a skipped change's dependents are not run.
// A step serving no change (an explicit one in a test) is not idle.
func (s *actionStep) idle(env *apply.Env) bool {
	if len(s.serves) == 0 {
		return false
	}
	for _, id := range s.serves {
		if !skipped(env, id) {
			return false
		}
	}
	return true
}

// done marks the served changes done, never over a skipped outcome.
func (s *actionStep) done(env *apply.Env) apply.Result {
	res := apply.Result{Outcomes: map[string]apply.Outcome{}}
	for _, id := range s.serves {
		if !skipped(env, id) {
			res.Outcomes[id] = apply.Outcome{Status: "done"}
		}
	}
	return res
}

type redeployStep struct {
	actionStep
	wait bool
}

// NewRedeployStep redeploys a place with its current variables. wait marks a
// redeploy of a database password's group, done only once the new
// deployment reports SUCCESS; it is frozen with the run.
func NewRedeployStep(p domain.Place, serves, deps []string, wait bool) apply.Step {
	return &redeployStep{actionStep: actionStep{prefix: "redeploy", place: p, serves: serves, deps: deps}, wait: wait}
}

// WaitsForSuccess implements apply.SuccessWaiter.
func (s *redeployStep) WaitsForSuccess() bool { return s.wait }

func inProgress(ds []railway.Deployment) bool {
	for _, d := range ds {
		if inProgressRE.MatchString(d.Status) {
			return true
		}
	}
	return false
}

// Run redeploys; when Railway refuses because a build is still running it
// tries again every 15 s, up to 10 minutes. Refused with
// no build in progress, and none seen before, it gives up at once. A step
// that waits for SUCCESS is done only once its new deployment is.
func (s *redeployStep) Run(ctx context.Context, env *apply.Env, _ apply.StepRecord) (apply.Result, error) {
	if s.idle(env) {
		return apply.Result{}, nil
	}
	var before map[string]bool
	if s.wait {
		var slept time.Duration
		ds, err := s.deployments(ctx, env, &slept)
		if err != nil {
			return apply.Result{}, err
		}
		before = make(map[string]bool, len(ds))
		for _, d := range ds {
			before[d.ID] = true
		}
	}
	var waited time.Duration
	building := false // a build was seen in progress during this run
	for {
		called := envNow(env)
		err := env.API.Redeploy(ctx, s.place.EnvID, s.place.ServiceID)
		if err == nil {
			if s.wait {
				if err := s.waitSuccess(ctx, env, before, called); err != nil {
					return apply.Result{}, err
				}
			}
			return s.done(env), nil
		}
		if !busyRE.MatchString(err.Error()) {
			return apply.Result{}, err
		}
		// Wait while a build is in progress, reading deployments every 15 s.
		// Refused right after a build finished, wait one more tick.
		ticked := false
		for {
			ds, err := env.API.Deployments(ctx, s.place.EnvID, s.place.ServiceID)
			if err != nil {
				return apply.Result{}, err
			}
			if inProgress(ds) {
				building = true
			} else if !building {
				return apply.Result{}, errors.New("Railway refuses to redeploy: the service's last deployment never finished building · deploy it once from the dashboard, then run apply again")
			} else if ticked {
				break
			}
			if waited >= busyTimeout {
				return apply.Result{}, errors.New("still building after 10 minutes · run apply again later")
			}
			if err := env.Sleep(ctx, busyPoll); err != nil {
				return apply.Result{}, err
			}
			waited += busyPoll
			ticked = true
		}
	}
}

func envNow(env *apply.Env) time.Time {
	if env.Now != nil {
		return env.Now()
	}
	return time.Now()
}

// deployReadTries is how many times a password group's redeploy reads the
// deployments before a failed read fails the step.
const deployReadTries = 3

// deployments reads the service's deployments, again after a failure, up
// to deployReadTries times deployPoll apart; the time slept is added to
// *waited.
func (s *redeployStep) deployments(ctx context.Context, env *apply.Env, waited *time.Duration) ([]railway.Deployment, error) {
	for try := 1; ; try++ {
		ds, err := env.API.Deployments(ctx, s.place.EnvID, s.place.ServiceID)
		if err == nil || try >= deployReadTries || ctx.Err() != nil {
			return ds, err
		}
		if err := env.Sleep(ctx, deployPoll); err != nil {
			return nil, err
		}
		*waited += deployPoll
	}
}

// waitSuccess polls the service's deployments until the new one, the
// newest not listed before the call and created at or after it), is SUCCESS,
// or SLEEPING (a serverless service that started and went idle): FAILED,
// CRASHED, REMOVED, REMOVING or SKIPPED fails, and so does waiting 10
// minutes, naming the last status (e.g. NEEDS_APPROVAL). A failed read is
// retried twice (deployments); the retry sleeps count toward the 10 minutes.
func (s *redeployStep) waitSuccess(ctx context.Context, env *apply.Env, before map[string]bool, called time.Time) error {
	status := "not listed yet"
	for waited := time.Duration(0); ; waited += deployPoll {
		ds, err := s.deployments(ctx, env, &waited)
		if err != nil {
			return err
		}
		var found *railway.Deployment
		for i, d := range ds {
			if before[d.ID] || d.CreatedAt.Before(called.Add(-deploySkew)) {
				continue
			}
			if found == nil || d.CreatedAt.After(found.CreatedAt) {
				found = &ds[i]
			}
		}
		if found != nil {
			status = text.Sanitize(found.Status)
			switch {
			case found.Status == "SUCCESS" || found.Status == "SLEEPING":
				return nil
			case deadRE.MatchString(found.Status):
				return fmt.Errorf("new deployment %s · see its logs, then continue", status)
			}
		}
		if waited >= deployTimeout {
			return fmt.Errorf("new deployment not SUCCESS after 10 minutes (%s) · continue redeploys again", status)
		}
		if err := env.Sleep(ctx, deployPoll); err != nil {
			return err
		}
	}
}

type restartStep struct{ actionStep }

// NewRestartStep restarts the running deployment of a place.
func NewRestartStep(p domain.Place, serves []string) apply.Step {
	return &restartStep{actionStep{prefix: "restart", place: p, serves: serves}}
}

func (s *restartStep) Run(ctx context.Context, env *apply.Env, _ apply.StepRecord) (apply.Result, error) {
	if s.idle(env) {
		return apply.Result{}, nil
	}
	id, err := env.API.ActiveDeployment(ctx, s.place.EnvID, s.place.ServiceID)
	if err != nil {
		return apply.Result{}, err
	}
	if id == "" {
		return apply.Result{}, errors.New("nothing running to restart")
	}
	if err := env.API.RestartDeployment(ctx, s.place.EnvID, id); err != nil {
		return apply.Result{}, err
	}
	return s.done(env), nil
}
