package kinds

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
	"github.com/men232/railway-manager/internal/store"
)

// volumeStore is the fixture store with volume v1 (5000 MB) mounted in api and
// v2 mounted in db.
func volumeStore(t *testing.T, online bool) *store.Store {
	t.Helper()
	s := fixtureStore(t)
	s.SetConfig("e1", railway.EnvConfig{Volumes: map[string]railway.VolumeConfig{
		"v1": {SizeMB: 5000, AllowOnlineResize: online},
		"v2": {SizeMB: 100},
	}})
	s.ReplaceVolumes([]string{"e1"}, []railway.VolumeMount{
		{EnvID: "e1", ServiceID: "api", VolumeID: "v1", MountPath: "/data", SizeMB: 5000},
		{EnvID: "e1", ServiceID: "db", VolumeID: "v2", MountPath: "/db", SizeMB: 100},
	})
	return s
}

func mustResize(t *testing.T, s *store.Store, svc, vol string, mb int) changeset.Change {
	t.Helper()
	c, err := ResizeVolume(s, place(svc), vol, mb, now)
	if err != nil {
		t.Fatalf("ResizeVolume: %v", err)
	}
	return c
}

func TestResizeRefusesShrink(t *testing.T) {
	s := volumeStore(t, false)
	for _, mb := range []int{5000, 4999, 0, -1} {
		if _, err := ResizeVolume(s, place("api"), "v1", mb, now); err == nil || !strings.Contains(err.Error(), "a volume can only grow") {
			t.Errorf("%d MB: err = %v", mb, err)
		}
	}
	c := mustResize(t, s, "api", "v1", 10000)
	if c.Kind != "volume.size" || c.Target != "volume:v1" || c.Place != place("api") || !c.Created.Equal(now) {
		t.Errorf("change = %+v", c)
	}
	if string(c.New) != `{"volumeId":"v1","sizeMB":10000}` || c.Base.Value != "5000" {
		t.Errorf("New = %s, Base = %q", c.New, c.Base.Value)
	}
	k := NewRegistry()["volume.size"]
	if k == nil {
		t.Fatal("volume.size not registered")
	}
	if k.IsNoop(c) || k.Secret(c) || k.Validate(s, c) != nil {
		t.Error("IsNoop/Secret/Validate")
	}
	if got := k.Writes(c); !reflect.DeepEqual(got, []string{"volume:e1/v1"}) {
		t.Errorf("Writes = %v", got)
	}
	if k.Reads(c) != nil {
		t.Error("Reads not nil")
	}
	if o := k.Overlay(c); o.Short != "disk 5.0 GB → 10.0 GB · can't be undone" || o.Tone != "warn" {
		t.Errorf("Overlay = %+v", o)
	}
}

func TestResizeRefusesForeignVolume(t *testing.T) {
	s := volumeStore(t, false)
	if _, err := ResizeVolume(s, place("api"), "v2", 500, now); err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Errorf("foreign: %v", err)
	}
	if _, err := ResizeVolume(s, place("api"), "nope", 500, now); err == nil {
		t.Error("unknown volume accepted")
	}
	if _, err := ResizeVolume(s, place(""), "v1", 9000, now); err == nil {
		t.Error("shared place accepted")
	}
	if _, err := ResizeVolume(fixtureStore(t), place("api"), "v1", 9000, now); err == nil {
		t.Error("unloaded config accepted")
	}
	// Mounted but missing from the config.
	s2 := fixtureStore(t)
	s2.SetConfig("e1", railway.EnvConfig{})
	s2.ReplaceVolumes([]string{"e1"}, []railway.VolumeMount{{EnvID: "e1", ServiceID: "api", VolumeID: "v1"}})
	if _, err := ResizeVolume(s2, place("api"), "v1", 9000, now); err == nil {
		t.Error("volume absent from config accepted")
	}
}

func TestVolumeValidateStrictDecode(t *testing.T) {
	s := volumeStore(t, false)
	k := NewRegistry()["volume.size"]
	c := mustResize(t, s, "api", "v1", 10000)
	for _, bad := range []string{``, `{`, `[]`, `{"volumeId":"v1","sizeMB":"x"}`, `{"volumeId":"v1","sizeMB":0}`, `{"volumeId":"","sizeMB":9}`, `{"volumeId":"v2","sizeMB":9}`} {
		b := c
		b.New = []byte(bad)
		if err := k.Validate(s, b); err == nil {
			t.Errorf("New %q accepted", bad)
		}
	}
}

func TestLimitsValidateStrictDecode(t *testing.T) {
	s := limitsStore(t)
	k := NewRegistry()["limits.set"]
	c := mustLimits(t, s, "api", fptr(16), iptr(16*gb))
	for _, bad := range []string{``, `{`, `[]`, `{"cpu":"x","memoryBytes":1}`} {
		b := c
		b.New = []byte(bad)
		if err := k.Validate(s, b); err == nil {
			t.Errorf("New %q accepted", bad)
		}
	}
	if err := k.Validate(s, c); err != nil {
		t.Errorf("good New refused: %v", err)
	}
}

func TestVolumePatchAndConfirm(t *testing.T) {
	s := volumeStore(t, false)
	c := mustResize(t, s, "api", "v1", 10000)
	k := NewRegistry()["volume.size"]
	frag, ok := k.Patch(c)
	want := map[string]any{"volumes": map[string]any{"v1": map[string]any{"sizeMB": 10000}}}
	if !ok || !reflect.DeepEqual(frag, want) {
		t.Errorf("Patch = %#v, %v", frag, ok)
	}
	cf := k.(Confirmer)
	for _, tc := range []struct {
		name     string
		size     int
		want     apply.Drift
		confirms bool
	}{
		{"base", 5000, apply.DriftNone, false},
		{"new", 10000, apply.DriftAlreadyNew, true},
		{"bigger", 20000, apply.DriftAlreadyNew, true},
		{"between", 7000, apply.DriftChanged, false},
	} {
		f := volumeFake(t, tc.size)
		env, _ := newEnv(t, f, s)
		d, err := k.Check(context.Background(), env, c)
		if err != nil || d != tc.want {
			t.Errorf("%s: Check = %v, %v", tc.name, d, err)
		}
		ok, err := cf.Confirm(context.Background(), env, c)
		if err != nil || ok != tc.confirms {
			t.Errorf("%s: Confirm = %v, %v", tc.name, ok, err)
		}
	}

	// Through the patch step the fake applies the size and the read-back confirms.
	f := volumeFake(t, 5000)
	res, _, _, err := runPatch(t, f, s, NewRegistry(), apply.StepRecord{}, c)
	if err != nil || res.Outcomes[c.ID].Status != "done" {
		t.Errorf("patch step: %+v, %v", res.Outcomes, err)
	}
}

func TestVolumeRedeploysOnlyWhenOffline(t *testing.T) {
	k := NewRegistry()["volume.size"]
	off := volumeStore(t, false)
	if got := k.Redeploys(off, mustResize(t, off, "api", "v1", 10000)); !reflect.DeepEqual(got, []domain.Place{place("api")}) {
		t.Errorf("offline Redeploys = %v", got)
	}
	on := volumeStore(t, true)
	if got := k.Redeploys(on, mustResize(t, on, "api", "v1", 10000)); got != nil {
		t.Errorf("online Redeploys = %v", got)
	}
}

func TestStrictDecodeTrailingAndUnknown(t *testing.T) {
	s := volumeStore(t, false)
	vk := NewRegistry()["volume.size"]
	c := mustResize(t, s, "api", "v1", 10000)
	for _, bad := range []string{`{"volumeId":"v1","sizeMB":9000} x`, `{"volumeId":"v1","sizeMB":9000}{}`, `{"volumeId":"v1","sizeMB":9000,"extra":1}`, `null`} {
		b := c
		b.New = []byte(bad)
		if vk.Validate(s, b) == nil {
			t.Errorf("volume New %q accepted", bad)
		}
	}
	ls := limitsStore(t)
	lk := NewRegistry()["limits.set"]
	l := mustLimits(t, ls, "api", fptr(16), iptr(16*gb))
	for _, bad := range []string{`{"cpu":16,"memoryBytes":16000000000} x`, `{"cpu":16,"memoryBytes":16000000000}{}`, `{"cpu":16,"memoryBytes":16000000000,"extra":1}`, `null`, `{}`, `{"cpu":16}`} {
		b := l
		b.New = []byte(bad)
		if lk.Validate(ls, b) == nil {
			t.Errorf("limits New %q accepted", bad)
		}
	}
	clr := mustLimits(t, ls, "api", nil, nil)
	if err := lk.Validate(ls, clr); err != nil || string(clr.New) != `{"cpu":null,"memoryBytes":null}` {
		t.Errorf("exact clear refused: %v, %s", err, clr.New)
	}
}

func TestCorruptNewNeverCommits(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
		mk   func(t *testing.T) (changeset.Change, *store.Store)
		bad  []string
		cfg  railway.EnvConfig
	}{
		{"limits", "limits.set", func(t *testing.T) (changeset.Change, *store.Store) {
			s := limitsStore(t)
			return mustLimits(t, s, "api", nil, nil), s
		}, []string{`{"cpu":"x","memoryBytes":1}`, `{`, `null`}, railway.EnvConfig{Services: map[string]railway.ServiceConfig{"api": {CPU: fptr(32), MemoryBytes: iptr(32 * gb)}}}},
		{"volume", "volume.size", func(t *testing.T) (changeset.Change, *store.Store) {
			s := volumeStore(t, false)
			return mustResize(t, s, "api", "v1", 10000), s
		}, []string{`{"volumeId":"v1","sizeMB":"x"}`, `{`}, railway.EnvConfig{Volumes: map[string]railway.VolumeConfig{"v1": {SizeMB: 5000}}}},
	} {
		for _, bad := range tc.bad {
			c, s := tc.mk(t)
			c.New = []byte(bad)
			f := newFake(t)
			f.SetEnvConfig("e1", tc.cfg)
			_, _, _, err := runPatch(t, f, s, NewRegistry(), apply.StepRecord{}, c)
			if err == nil {
				t.Errorf("%s %q: step succeeded", tc.name, bad)
			}
			if n := len(commits(f)); n != 0 {
				t.Errorf("%s %q: %d commits", tc.name, bad, n)
			}
		}
	}
}

// volumeFake is a fake with the volumes of volumeStore mounted and sized.
func volumeFake(t *testing.T, v1MB int) *railwaytest.Fake {
	t.Helper()
	f := newFake(t)
	f.SetEnvConfig("e1", railway.EnvConfig{Volumes: map[string]railway.VolumeConfig{
		"v1": {SizeMB: v1MB}, "v2": {SizeMB: 100},
	}})
	f.SetVolumes("p1", []railway.VolumeMount{
		{EnvID: "e1", ServiceID: "api", VolumeID: "v1", MountPath: "/data", SizeMB: float64(v1MB)},
		{EnvID: "e1", ServiceID: "db", VolumeID: "v2", MountPath: "/db", SizeMB: 100},
	})
	return f
}

// A change from disk may be tampered with: Check refuses a volume that is
// not in the environment or not mounted in the change's service, and the
// patch step sends nothing.
func TestVolumeCheckRefusesForeignVolume(t *testing.T) {
	s := volumeStore(t, false)
	good := mustResize(t, s, "api", "v1", 10000)
	for _, tc := range []struct {
		name, vol string
	}{
		{"another service's volume", "v2"},
		{"unknown volume", "v9"},
	} {
		c := good
		c.New = []byte(`{"volumeId":"` + tc.vol + `","sizeMB":10000}`)
		c.Target = "volume:" + tc.vol
		f := volumeFake(t, 5000)
		env, _ := newEnv(t, f, s)
		if _, err := NewRegistry()["volume.size"].Check(context.Background(), env, c); err == nil {
			t.Errorf("%s: Check succeeded", tc.name)
		}
		if _, _, _, err := runPatch(t, f, s, NewRegistry(), apply.StepRecord{}, c); err == nil {
			t.Errorf("%s: patch step succeeded", tc.name)
		}
		if n := len(commits(f)); n != 0 {
			t.Errorf("%s: commits = %d", tc.name, n)
		}
	}
	// The target must name the volume the data does.
	c := good
	c.Target = "volume:v2"
	env, _ := newEnv(t, volumeFake(t, 5000), s)
	if _, err := NewRegistry()["volume.size"].Check(context.Background(), env, c); err == nil {
		t.Error("mismatched target: Check succeeded")
	}
	// The real change still checks.
	env, _ = newEnv(t, volumeFake(t, 5000), s)
	if d, err := NewRegistry()["volume.size"].Check(context.Background(), env, good); err != nil || d != apply.DriftNone {
		t.Errorf("good: %v, %v", d, err)
	}
}
