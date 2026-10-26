package kinds

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
)

const gb = int64(1 << 30) // Railway's GB

func fptr(v float64) *float64 { return &v }
func iptr(v int64) *int64     { return &v }

// limitsStore is the fixture store with api's override at 32 vCPU / 32 GB (1024³).
func limitsStore(t *testing.T) *store.Store {
	t.Helper()
	s := fixtureStore(t)
	s.SetConfig("e1", railway.EnvConfig{Services: map[string]railway.ServiceConfig{
		"api": {CPU: fptr(32), MemoryBytes: iptr(32 * gb)},
	}})
	return s
}

func mustLimits(t *testing.T, s *store.Store, svc string, cpu *float64, mem *int64) changeset.Change {
	t.Helper()
	c, err := SetLimits(s, place(svc), cpu, mem, now)
	if err != nil {
		t.Fatalf("SetLimits(%s): %v", svc, err)
	}
	return c
}

func TestSetLimitsResolvesBase(t *testing.T) {
	s := limitsStore(t)
	c := mustLimits(t, s, "api", fptr(16), iptr(16*gb))
	if c.Kind != "limits.set" || c.Target != "limits" || c.Place != place("api") || !c.Created.Equal(now) {
		t.Errorf("change = %+v", c)
	}
	if string(c.New) != `{"cpu":16,"memoryBytes":17179869184}` {
		t.Errorf("New = %s", c.New)
	}
	if c.Base.Value != `{"cpu":32,"memoryBytes":34359738368}` {
		t.Errorf("Base.Value = %q", c.Base.Value)
	}
	k := NewRegistry()["limits.set"]
	if k == nil {
		t.Fatal("limits.set not registered")
	}
	if k.IsNoop(c) || k.Secret(c) {
		t.Errorf("IsNoop/Secret = %v/%v", k.IsNoop(c), k.Secret(c))
	}
	if got := k.Writes(c); !reflect.DeepEqual(got, []string{"limits:e1/api"}) {
		t.Errorf("Writes = %v", got)
	}
	if got := k.Reads(c); got != nil {
		t.Errorf("Reads = %v", got)
	}
	if got := k.Redeploys(s, c); !reflect.DeepEqual(got, []domain.Place{place("api")}) {
		t.Errorf("Redeploys = %v", got)
	}
	if err := k.Validate(s, c); err != nil {
		t.Errorf("Validate = %v", err)
	}
	if o := k.Overlay(c); o.Short != "cpu 32 → 16 · mem 32.0 GB → 16.0 GB" || o.Tone != "new" {
		t.Errorf("Overlay = %+v", o)
	}

	// No override: base is both null; clearing it is a no-op.
	none := mustLimits(t, s, "web", nil, nil)
	if none.Base.Value != `{"cpu":null,"memoryBytes":null}` || string(none.New) != `{"cpu":null,"memoryBytes":null}` {
		t.Errorf("none = %q / %s", none.Base.Value, none.New)
	}
	if !k.IsNoop(none) {
		t.Error("clearing no override is not a no-op")
	}
	if o := k.Overlay(none); o.Short != "limits → plan default" {
		t.Errorf("clear Overlay = %+v", o)
	}
	same := mustLimits(t, s, "api", fptr(32), iptr(32*gb))
	if !k.IsNoop(same) {
		t.Error("setting the current override is not a no-op")
	}
	set := mustLimits(t, s, "web", fptr(2), iptr(1_717_986_918)) // 1.6 GB (1024³)
	if o := k.Overlay(set); o.Short != "cpu default → 2 · mem default → 1.6 GB" {
		t.Errorf("set-from-default Overlay = %+v", o)
	}
	small := mustLimits(t, s, "web", fptr(1), iptr(512<<20))
	if o := k.Overlay(small); o.Short != "cpu default → 1 · mem default → 512.0 MB" {
		t.Errorf("sub-GB Overlay = %+v", o)
	}

	for _, tc := range []struct {
		name string
		cpu  *float64
		mem  *int64
		want string
	}{
		{"zero cpu", fptr(0), iptr(gb), "cpu"},
		{"negative cpu", fptr(-1), iptr(gb), "cpu"},
		{"small memory", fptr(1), iptr(268435455), "256 MB"},
		{"cpu only", fptr(1), nil, "both"},
		{"memory only", nil, iptr(gb), "both"},
	} {
		if _, err := SetLimits(s, place("api"), tc.cpu, tc.mem, now); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
	if _, err := SetLimits(s, place("api"), fptr(1), iptr(268435456), now); err != nil {
		t.Errorf("256 MB refused: %v", err)
	}
	if _, err := SetLimits(s, place(""), fptr(1), iptr(gb), now); err == nil {
		t.Error("shared place accepted")
	}
	if _, err := SetLimits(fixtureStore(t), place("api"), fptr(1), iptr(gb), now); err == nil || err.Error() != "environment settings not loaded yet · r refresh" {
		t.Errorf("unloaded config err = %v", err)
	}
}

func TestLimitsPatchFragment(t *testing.T) {
	c := mustLimits(t, limitsStore(t), "api", fptr(16), iptr(16*gb))
	frag, ok := NewRegistry()["limits.set"].Patch(c)
	want := map[string]any{"services": map[string]any{"api": map[string]any{"deploy": map[string]any{
		"limitOverride": map[string]any{"containers": map[string]any{"cpu": 16.0, "memoryBytes": 16 * gb}},
	}}}}
	if !ok || !reflect.DeepEqual(frag, want) {
		t.Errorf("Patch = %#v, %v", frag, ok)
	}
}

func TestLimitsClearUsesContainersNull(t *testing.T) {
	c := mustLimits(t, limitsStore(t), "api", nil, nil)
	frag, ok := NewRegistry()["limits.set"].Patch(c)
	want := map[string]any{"services": map[string]any{"api": map[string]any{"deploy": map[string]any{
		"limitOverride": map[string]any{"containers": nil},
	}}}}
	if !ok || !reflect.DeepEqual(frag, want) {
		t.Errorf("Patch = %#v, %v", frag, ok)
	}
	raw, _ := json.Marshal(frag)
	if strings.Contains(string(raw), `"limitOverride":null`) {
		t.Errorf("clear sends limitOverride null: %s", raw)
	}
}

func TestLimitsCheck(t *testing.T) {
	s := limitsStore(t)
	set := mustLimits(t, s, "api", fptr(16), iptr(16*gb))
	clr := mustLimits(t, s, "api", nil, nil)
	k := NewRegistry()["limits.set"].(Confirmer)
	for _, tc := range []struct {
		name     string
		railway  *railway.ServiceConfig // nil: no override
		c        changeset.Change
		want     apply.Drift
		confirms bool
	}{
		{"set, none", nil, set, apply.DriftChanged, false},
		{"set, base", &railway.ServiceConfig{CPU: fptr(32), MemoryBytes: iptr(32 * gb)}, set, apply.DriftNone, false},
		{"set, new", &railway.ServiceConfig{CPU: fptr(16), MemoryBytes: iptr(16 * gb)}, set, apply.DriftAlreadyNew, true},
		{"set, other", &railway.ServiceConfig{CPU: fptr(8), MemoryBytes: iptr(16 * gb)}, set, apply.DriftChanged, false},
		{"clear, none", nil, clr, apply.DriftAlreadyNew, true},
		{"clear, base", &railway.ServiceConfig{CPU: fptr(32), MemoryBytes: iptr(32 * gb)}, clr, apply.DriftNone, false},
		{"clear, other", &railway.ServiceConfig{CPU: fptr(32)}, clr, apply.DriftChanged, false},
	} {
		f := newFake(t)
		cfg := railway.EnvConfig{Services: map[string]railway.ServiceConfig{}}
		if tc.railway != nil {
			cfg.Services["api"] = *tc.railway
		}
		f.SetEnvConfig("e1", cfg)
		env, _ := newEnv(t, f, s)
		d, err := NewRegistry()["limits.set"].Check(context.Background(), env, tc.c)
		if err != nil || d != tc.want {
			t.Errorf("%s: Check = %v, %v; want %v", tc.name, d, err, tc.want)
		}
		ok, err := k.Confirm(context.Background(), env, tc.c)
		if err != nil || ok != tc.confirms {
			t.Errorf("%s: Confirm = %v, %v; want %v", tc.name, ok, err, tc.confirms)
		}
	}
}

// nullOverrideKind sends the trap the live API showed: limitOverride null,
// which Railway commits and ignores.
type nullOverrideKind struct{ limitsKind }

func (nullOverrideKind) Patch(c changeset.Change) (map[string]any, bool) {
	return map[string]any{"services": map[string]any{c.Place.ServiceID: map[string]any{"deploy": map[string]any{"limitOverride": nil}}}}, true
}

func TestLimitsSilentNoopFailsConfirm(t *testing.T) {
	s := limitsStore(t)
	f := newFake(t)
	f.SetEnvConfig("e1", railway.EnvConfig{Services: map[string]railway.ServiceConfig{
		"api": {CPU: fptr(32), MemoryBytes: iptr(32 * gb)},
	}})
	c := mustLimits(t, s, "api", nil, nil)
	_, _, _, err := runPatch(t, f, s, apply.Registry{"limits.set": nullOverrideKind{}}, apply.StepRecord{}, c)
	if err == nil || err.Error() != "not in Railway after commit: limits at e1/api" {
		t.Errorf("err = %v", err)
	}
	if n := len(commits(f)); n != 1 {
		t.Errorf("commits = %d, want 1", n)
	}

	// The real kind clears it.
	f2 := newFake(t)
	f2.SetEnvConfig("e1", railway.EnvConfig{Services: map[string]railway.ServiceConfig{
		"api": {CPU: fptr(32), MemoryBytes: iptr(32 * gb)},
	}})
	res, _, _, err := runPatch(t, f2, s, NewRegistry(), apply.StepRecord{}, c)
	if err != nil || res.Outcomes[c.ID].Status != "done" {
		t.Errorf("clear: %+v, %v", res.Outcomes, err)
	}
}

func TestLimitsThroughPatchStep(t *testing.T) {
	s := limitsStore(t)
	f := newFake(t)
	f.SetEnvConfig("e1", railway.EnvConfig{Services: map[string]railway.ServiceConfig{
		"api": {CPU: fptr(32), MemoryBytes: iptr(32 * gb)},
	}})
	l := mustLimits(t, s, "api", fptr(16), iptr(16*gb))
	v := mustSet(t, s, place("api"), "PLAIN", "two")
	res, _, _, err := runPatch(t, f, s, NewRegistry(), apply.StepRecord{}, l, v)
	if err != nil {
		t.Fatal(err)
	}
	cm := commits(f)
	if len(cm) != 1 {
		t.Fatalf("commits = %d, want 1", len(cm))
	}
	want := map[string]any{"services": map[string]any{"api": map[string]any{
		"deploy":    map[string]any{"limitOverride": map[string]any{"containers": map[string]any{"cpu": 16.0, "memoryBytes": float64(16 * gb)}}},
		"variables": map[string]any{"PLAIN": map[string]any{"value": "two"}},
	}}}
	if !reflect.DeepEqual(cm[0].Vars["patch"], want) {
		t.Errorf("patch = %#v", cm[0].Vars["patch"])
	}
	if res.Outcomes[l.ID].Status != "done" || res.Outcomes[v.ID].Status != "done" {
		t.Errorf("outcomes = %+v", res.Outcomes)
	}
	if got, _ := f.Var("e1", "api", "PLAIN"); got != "two" {
		t.Errorf("PLAIN = %q", got)
	}
}

// TestLimitsBadDataNeverPanicsOrSends: change data from disk is never
// trusted. Compile (which calls Patch first) must not panic on a half-null
// or out-of-range override, and the run must fail before anything is sent.
func TestLimitsBadDataNeverPanicsOrSends(t *testing.T) {
	for _, bad := range []string{
		`{"cpu":2,"memoryBytes":null}`,
		`{"cpu":null,"memoryBytes":1073741824}`,
		`{"cpu":-1,"memoryBytes":1073741824}`,
		`{"cpu":0,"memoryBytes":1073741824}`,
		`{"cpu":2,"memoryBytes":1}`,
	} {
		s := limitsStore(t)
		f := newFake(t)
		f.SetEnvConfig("e1", railway.EnvConfig{Services: map[string]railway.ServiceConfig{
			"api": {CPU: fptr(32), MemoryBytes: iptr(32 * gb)},
		}})
		c := mustLimits(t, s, "api", fptr(16), iptr(16*gb))
		c.New = json.RawMessage(bad)
		reg := NewRegistry()
		cs := changeset.ChangeSet{Changes: []changeset.Change{c}}
		var g apply.Graph
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s: Compile panicked: %v", bad, r)
				}
			}()
			var err error
			g, err = apply.Compile(cs, s, reg,
				func(envID string, changes []changeset.Change) apply.Step {
					return NewPatchStep(envID, changes, reg, "p1")
				}, NewRedeployStep)
			if err != nil {
				t.Fatalf("%s: Compile: %v", bad, err)
			}
		}()
		st := g.Steps["patch:e1"]
		if st == nil {
			t.Fatalf("%s: no patch step: %v", bad, g.Order)
		}
		env, _ := newEnv(t, f, s)
		if _, err := st.Run(context.Background(), env, apply.StepRecord{}); err == nil {
			t.Errorf("%s: patch step succeeded", bad)
		}
		if _, err := reg["limits.set"].Check(context.Background(), env, c); err == nil {
			t.Errorf("%s: Check succeeded", bad)
		}
		if n := len(commits(f)); n != 0 {
			t.Errorf("%s: commits = %d, want 0", bad, n)
		}
	}
}
