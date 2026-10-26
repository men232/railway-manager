package kinds

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
)

const busyMsg = "Cannot redeploy yet, please wait for the original deployment to finish building"

func TestRedeployConstructors(t *testing.T) {
	reg := NewRegistry()
	r := Redeploy(place("api"), now)
	if r.Kind != "service.redeploy" || r.Target != "redeploy" || r.ID == "" || !r.Created.Equal(now) {
		t.Errorf("Redeploy = %+v", r)
	}
	own := reg[r.Kind].OwnSteps(r)
	if len(own) != 1 || own[0].Key() != "redeploy:e1/api" || own[0].Lock() != "e1/api" || !reflect.DeepEqual(own[0].Serves(), []string{r.ID}) {
		t.Errorf("redeploy own steps = %v", own)
	}
	x := Restart(place("api"), now)
	if x.Kind != "service.restart" || x.Target != "restart" {
		t.Errorf("Restart = %+v", x)
	}
	own = reg[x.Kind].OwnSteps(x)
	if len(own) != 1 || own[0].Key() != "restart:e1/api" || own[0].Lock() != "e1/api" {
		t.Errorf("restart own steps = %v", own)
	}
	if err := reg[x.Kind].Validate(fixtureStore(t), Restart(place(""), now)); err == nil {
		t.Error("a restart of the shared scope should be refused")
	}
}

func TestRedeployBusyWaits(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	f.FailNext("Redeploy", busyMsg)
	f.RedeployBusy(2)
	env, sl := newEnv(t, f, s)
	st := NewRedeployStep(place("api"), []string{"c1"}, []string{"patch:e1"}, false)
	if st.Key() != "redeploy:e1/api" || st.Lock() != "e1/api" || !reflect.DeepEqual(st.Deps(), []string{"patch:e1"}) {
		t.Errorf("step = %s %s %v", st.Key(), st.Lock(), st.Deps())
	}
	res, err := st.Run(context.Background(), env, apply.StepRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sl.got, []time.Duration{15 * time.Second, 15 * time.Second}) {
		t.Errorf("sleeps = %v", sl.got)
	}
	if res.Outcomes["c1"].Status != "done" {
		t.Errorf("outcome = %+v", res.Outcomes)
	}
	var n int
	for _, m := range f.Mutations() {
		if m.Op == "Redeploy" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("served redeploys = %d, want 1", n)
	}

	// Refused again right after the build finished: one more tick, no
	// "never finished building".
	f = newFake(t)
	f.FailNext("Redeploy", busyMsg)
	f.FailNext("Redeploy", busyMsg)
	f.RedeployBusy(1)
	env, sl = newEnv(t, f, s)
	if _, err := NewRedeployStep(place("api"), []string{"c1"}, nil, false).Run(context.Background(), env, apply.StepRecord{}); err != nil {
		t.Fatalf("second refusal: %v", err)
	}
	if !reflect.DeepEqual(sl.got, []time.Duration{15 * time.Second, 15 * time.Second}) {
		t.Errorf("sleeps = %v", sl.got)
	}

	// Busy with nothing in progress.
	f = newFake(t)
	f.FailNext("Redeploy", busyMsg)
	env, sl = newEnv(t, f, s)
	_, err = NewRedeployStep(place("api"), []string{"c1"}, nil, false).Run(context.Background(), env, apply.StepRecord{})
	want := "Railway refuses to redeploy: the service's last deployment never finished building · deploy it once from the dashboard, then run apply again"
	if err == nil || err.Error() != want {
		t.Errorf("error = %v", err)
	}
	if len(sl.got) != 0 {
		t.Errorf("sleeps = %v", sl.got)
	}

	// Still building after 10 minutes.
	f = newFake(t)
	f.FailNext("Redeploy", busyMsg)
	f.RedeployBusy(1000)
	env, sl = newEnv(t, f, s)
	_, err = NewRedeployStep(place("api"), []string{"c1"}, nil, false).Run(context.Background(), env, apply.StepRecord{})
	if err == nil || err.Error() != "still building after 10 minutes · run apply again later" {
		t.Errorf("error = %v", err)
	}
	if len(sl.got) != 40 {
		t.Errorf("sleeps = %d, want 40", len(sl.got))
	}

	// Another error passes through.
	f = newFake(t)
	f.FailNext("Redeploy", "service gone")
	env, _ = newEnv(t, f, s)
	if _, err = NewRedeployStep(place("api"), nil, nil, false).Run(context.Background(), env, apply.StepRecord{}); err == nil {
		t.Error("expected an error")
	}
}

func TestRestartStep(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	f.SetActiveDeployment("e1", "api", "d1")
	env, _ := newEnv(t, f, s)
	st := NewRestartStep(place("api"), []string{"c1"})
	res, err := st.Run(context.Background(), env, apply.StepRecord{})
	if err != nil {
		t.Fatal(err)
	}
	m := f.Mutations()
	if len(m) != 1 || m[0].Op != "RestartDeployment" || m[0].Vars["id"] != "d1" {
		t.Errorf("mutations = %+v", m)
	}
	if res.Outcomes["c1"].Status != "done" {
		t.Errorf("outcome = %+v", res.Outcomes)
	}

	f = newFake(t)
	env, _ = newEnv(t, f, s)
	_, err = NewRestartStep(place("api"), []string{"c1"}).Run(context.Background(), env, apply.StepRecord{})
	if err == nil || err.Error() != "nothing running to restart" {
		t.Errorf("error = %v", err)
	}
}

func TestActionStepsSkipSkippedChanges(t *testing.T) {
	s := fixtureStore(t)
	for _, tc := range []struct {
		name string
		step func(serves []string) apply.Step
		op   string
	}{
		{"redeploy", func(serves []string) apply.Step { return NewRedeployStep(place("api"), serves, nil, false) }, "Redeploy"},
		{"restart", func(serves []string) apply.Step { return NewRestartStep(place("api"), serves) }, "RestartDeployment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.SetActiveDeployment("e1", "api", "d1")
			env, _ := newEnv(t, f, s)
			skip := apply.Outcome{Status: "skipped", Reason: "changed in Railway since the change was made"}
			for _, id := range []string{"c1", "c2"} {
				if err := env.Journal.SetOutcome(id, skip); err != nil {
					t.Fatal(err)
				}
			}

			// Only skipped changes: nothing to deploy.
			res, err := tc.step([]string{"c1", "c2"}).Run(context.Background(), env, apply.StepRecord{})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Outcomes) != 0 || len(f.Mutations()) != 0 {
				t.Errorf("all skipped: outcomes %v, mutations %+v; want none", res.Outcomes, f.Mutations())
			}

			// Mixed: it acts, and leaves the skipped outcome alone.
			res, err = tc.step([]string{"c1", "c3"}).Run(context.Background(), env, apply.StepRecord{})
			if err != nil {
				t.Fatal(err)
			}
			if m := f.Mutations(); len(m) != 1 || m[0].Op != tc.op {
				t.Errorf("mixed: mutations = %+v", m)
			}
			if _, ok := res.Outcomes["c1"]; ok || res.Outcomes["c3"].Status != "done" {
				t.Errorf("mixed: outcomes = %v, want only c3 done", res.Outcomes)
			}
		})
	}
}

// A redeploy of a password group waits until its new deployment is SUCCESS.
func TestRedeployWaitsForSuccess(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	// An older SUCCESS deployment is never taken for the new one.
	f.AddDeployment("e1", "api", "dep-old", "SUCCESS", time.Now().Add(-time.Hour))
	f.DeployStatuses("e1", "api", "BUILDING", "DEPLOYING", "SUCCESS")
	env, sl := newEnv(t, f, s)
	env.Now = time.Now
	res, err := NewRedeployStep(place("api"), []string{"c1"}, nil, true).Run(context.Background(), env, apply.StepRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcomes["c1"].Status != "done" {
		t.Errorf("outcome = %+v", res.Outcomes)
	}
	if len(sl.got) != 2 {
		t.Errorf("sleeps = %v, want two polls before SUCCESS", sl.got)
	}

	for _, bad := range []string{"FAILED", "CRASHED", "REMOVED"} {
		f := newFake(t)
		f.DeployStatuses("e1", "api", "DEPLOYING", bad)
		env, _ := newEnv(t, f, s)
		env.Now = time.Now
		_, err := NewRedeployStep(place("api"), []string{"c1"}, nil, true).Run(context.Background(), env, apply.StepRecord{})
		if err == nil || !strings.Contains(err.Error(), bad) {
			t.Errorf("%s: err = %v", bad, err)
		}
	}

	// Never finishing: fails after 10 minutes of polls.
	f = newFake(t)
	f.DeployStatuses("e1", "api", "BUILDING")
	env, sl = newEnv(t, f, s)
	env.Now = time.Now
	_, err = NewRedeployStep(place("api"), []string{"c1"}, nil, true).Run(context.Background(), env, apply.StepRecord{})
	if err == nil || !strings.Contains(err.Error(), "10 minutes") {
		t.Errorf("timeout: err = %v", err)
	}
	var total time.Duration
	for _, d := range sl.got {
		total += d
	}
	if total < 10*time.Minute || total > 11*time.Minute {
		t.Errorf("waited %v", total)
	}

	// Without wait, done once Railway accepted it.
	f = newFake(t)
	f.DeployStatuses("e1", "api", "BUILDING")
	env, sl = newEnv(t, f, s)
	if _, err := NewRedeployStep(place("api"), []string{"c1"}, nil, false).Run(context.Background(), env, apply.StepRecord{}); err != nil || len(sl.got) != 0 {
		t.Errorf("no wait: err %v, sleeps %v", err, sl.got)
	}
}

func TestRedeployWaitEdges(t *testing.T) {
	s := fixtureStore(t)
	run := func(f *railwaytest.Fake) (*sleeps, error) {
		env, sl := newEnv(t, f, s)
		env.Now = time.Now
		_, err := NewRedeployStep(place("api"), []string{"c1"}, nil, true).Run(context.Background(), env, apply.StepRecord{})
		return sl, err
	}

	// A SUCCESS deployment from 30 s before the call, listed before it, is
	// not the new one.
	f := newFake(t)
	f.AddDeployment("e1", "api", "dep-recent", "SUCCESS", time.Now().Add(-30*time.Second))
	f.DeployStatuses("e1", "api", "BUILDING", "SUCCESS")
	if sl, err := run(f); err != nil || len(sl.got) != 1 {
		t.Errorf("recent listed deployment: err %v, sleeps %v", err, sl.got)
	}

	// SLEEPING is a started service gone idle: success.
	f = newFake(t)
	f.DeployStatuses("e1", "api", "SLEEPING")
	if _, err := run(f); err != nil {
		t.Errorf("SLEEPING: %v", err)
	}

	// SKIPPED fails at once; NEEDS_APPROVAL is named when the wait ends.
	f = newFake(t)
	f.DeployStatuses("e1", "api", "SKIPPED")
	if _, err := run(f); err == nil || !strings.Contains(err.Error(), "SKIPPED") {
		t.Errorf("SKIPPED: err = %v", err)
	}
	f = newFake(t)
	f.DeployStatuses("e1", "api", "NEEDS_APPROVAL")
	if _, err := run(f); err == nil || !strings.Contains(err.Error(), "NEEDS_APPROVAL") {
		t.Errorf("NEEDS_APPROVAL: err = %v", err)
	}

	// A failed read is tried again twice.
	f = newFake(t)
	f.FailNext("ServiceDeployments", "upstream hiccup")
	f.FailNext("ServiceDeployments", "upstream hiccup")
	if _, err := run(f); err != nil {
		t.Errorf("two failed reads: %v", err)
	}
	f = newFake(t)
	for range 3 {
		f.FailNext("ServiceDeployments", "upstream hiccup")
	}
	if _, err := run(f); err == nil {
		t.Error("three failed reads in a row must fail")
	}
}

func TestRedeployWaitRemovingAndBound(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	f.DeployStatuses("e1", "api", "DEPLOYING", "REMOVING")
	env, _ := newEnv(t, f, s)
	env.Now = time.Now
	if _, err := NewRedeployStep(place("api"), []string{"c1"}, nil, true).Run(context.Background(), env, apply.StepRecord{}); err == nil || !strings.Contains(err.Error(), "REMOVING") {
		t.Errorf("REMOVING: err = %v", err)
	}

	// Sleeps between failed reads count toward the 10 minutes.
	f = newFake(t)
	f.DeployStatuses("e1", "api", "BUILDING")
	env, sl := newEnv(t, f, s)
	env.Now = time.Now
	env.API = &flakyDeployments{RailwayAPI: env.API, fail: map[int]bool{3: true, 4: true}} // reads 3 and 4: during the wait
	_, err := NewRedeployStep(place("api"), []string{"c1"}, nil, true).Run(context.Background(), env, apply.StepRecord{})
	if err == nil || !strings.Contains(err.Error(), "10 minutes") {
		t.Fatalf("err = %v", err)
	}
	var total time.Duration
	for _, d := range sl.got {
		total += d
	}
	if total != 10*time.Minute {
		t.Errorf("slept %v, want 10m: retry sleeps count", total)
	}
}

// flakyDeployments fails the Deployments reads numbered in fail (1-based).
type flakyDeployments struct {
	apply.RailwayAPI
	n    int
	fail map[int]bool
}

func (f *flakyDeployments) Deployments(ctx context.Context, envID, serviceID string) ([]railway.Deployment, error) {
	f.n++
	if f.fail[f.n] {
		return nil, errors.New("upstream hiccup")
	}
	return f.RailwayAPI.Deployments(ctx, envID, serviceID)
}
