package kinds

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
	"github.com/men232/railway-manager/internal/store"
)

// domainFixture: api has railway domain sd-a and custom domain shop.example.com.
func domainFixture() railway.Domains {
	return railway.Domains{
		Service: []railway.ServiceDomain{{ID: "sd-a", Domain: "api-a.up.railway.test", Port: 8080}},
		Custom:  []railway.CustomDomain{{ID: "cd-a", Domain: "shop.example.com", Port: 8080}},
	}
}

func domainStore(t *testing.T) *store.Store {
	t.Helper()
	s := fixtureStore(t)
	s.SetDomains(place("api"), domainFixture())
	return s
}

func domainFake(t *testing.T) *railwaytest.Fake {
	t.Helper()
	f := newFake(t)
	f.SetDomains("e1", "api", domainFixture())
	return f
}

// domainMutations are the domain mutations the fake served.
func domainMutations(f *railwaytest.Fake) []string {
	var out []string
	for _, m := range f.Mutations() {
		if strings.Contains(m.Op, "Domain") && m.Op != "Domains" {
			out = append(out, m.Op)
		}
	}
	return out
}

// ownDomainStep returns the one own step of c.
func ownDomainStep(t *testing.T, c changeset.Change) apply.Step {
	t.Helper()
	k := NewRegistry()[c.Kind]
	if k == nil {
		t.Fatalf("%s not registered", c.Kind)
	}
	steps := k.OwnSteps(c)
	if len(steps) != 1 {
		t.Fatalf("OwnSteps = %d steps", len(steps))
	}
	return steps[0]
}

func runDomain(t *testing.T, env *apply.Env, c changeset.Change, prior apply.StepRecord) (apply.Result, error) {
	t.Helper()
	return ownDomainStep(t, c).Run(context.Background(), env, prior)
}

func wantDone(t *testing.T, res apply.Result, c changeset.Change) {
	t.Helper()
	if got := res.Outcomes[c.ID].Status; got != "done" {
		t.Errorf("outcome = %q, want done", got)
	}
}

func TestValidHost(t *testing.T) {
	for host, want := range map[string]bool{
		"shop.example.com":               true,
		"SHOP.Example.COM":               true,
		"a.b":                            true,
		"x-1.example.io":                 true,
		"example":                        false,
		"":                               false,
		"-a.example.com":                 false,
		"a-.example.com":                 false,
		"a..example.com":                 false,
		"a.example.com.":                 false,
		"a b.example.com":                false,
		"https://a.example.com":          false,
		"a.example.com/x":                false,
		strings.Repeat("a", 64) + ".com": false,
		strings.Repeat("a", 63) + ".com": true,
	} {
		if got := ValidHost(host); got != want {
			t.Errorf("ValidHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestAddCustomDomainValidates(t *testing.T) {
	s := domainStore(t)
	for _, tc := range []struct {
		host string
		port int
		want string
	}{
		{"not a host", 0, "host"},
		{"example", 0, "host"},
		{"new.example.com", -1, "port"},
		{"new.example.com", 65536, "port"},
		{"shop.example.com", 0, "already"},
		{"SHOP.example.com", 0, "already"},
	} {
		if _, err := AddCustomDomain(s, place("api"), tc.host, tc.port, now); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q :%d: err = %v, want %q", tc.host, tc.port, err, tc.want)
		}
	}
	if _, err := AddCustomDomain(s, place(""), "new.example.com", 0, now); err == nil {
		t.Error("shared place accepted")
	}
	c, err := AddCustomDomain(s, place("api"), "New.Example.com", 8080, now)
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != "domain.custom.add" || c.Target != "custom-domain:new.example.com" || c.Place != place("api") || !c.Created.Equal(now) {
		t.Errorf("change = %+v", c)
	}
	k := NewRegistry()[c.Kind]
	if k.Validate(s, c) != nil || k.IsNoop(c) || k.Secret(c) || k.Reads(c) != nil || k.Redeploys(s, c) != nil {
		t.Error("Validate/IsNoop/Secret/Reads/Redeploys")
	}
	if _, ok := k.Patch(c); ok {
		t.Error("Patch ok")
	}
	if got := k.Writes(c); !reflect.DeepEqual(got, []string{"domain:" + place("api").Key() + "/custom-domain:new.example.com"}) {
		t.Errorf("Writes = %v", got)
	}
	st := ownDomainStep(t, c)
	if st.Key() != k.Writes(c)[0] || st.Lock() != place("api").Key() || !reflect.DeepEqual(st.Serves(), []string{c.ID}) || len(st.Deps()) != 0 {
		t.Errorf("step key %q lock %q serves %v deps %v", st.Key(), st.Lock(), st.Serves(), st.Deps())
	}
	if o := k.Overlay(c); o.Short != "+ domain new.example.com → :8080" || o.Tone != "new" {
		t.Errorf("Overlay = %+v", o)
	}

	// Validate never trusts the constructor.
	for _, bad := range []string{`{`, `{"host":"new.example.com","port":8080,"x":1}`, `{"host":"bad host","port":0}`,
		`{"host":"other.example.com","port":0}`, `{"host":"new.example.com","port":70000}`, `{"host":"new.example.com"} {}`, `null`} {
		b := c
		b.New = json.RawMessage(bad)
		if k.Validate(s, b) == nil {
			t.Errorf("Validate accepted %s", bad)
		}
	}
}

func TestDomainConstructors(t *testing.T) {
	s := domainStore(t)
	add := AddServiceDomain(place("api"), 8080, now)
	if add.Kind != "domain.service.add" || add.Target != "service-domain:new" {
		t.Errorf("service add = %+v", add)
	}
	reg := NewRegistry()
	if o := reg[add.Kind].Overlay(add); o.Short != "+ railway domain → :8080" {
		t.Errorf("Overlay = %+v", o)
	}
	if err := reg[add.Kind].Validate(s, add); err != nil {
		t.Errorf("Validate: %v", err)
	}
	// At most one pending per place: a second add replaces the first.
	var cs changeset.ChangeSet
	for range 2 {
		if err := cs.Put(AddServiceDomain(place("api"), 8080, now), reg[add.Kind].IsNoop); err != nil {
			t.Fatal(err)
		}
	}
	if len(cs.Changes) != 1 {
		t.Errorf("%d pending service domain adds", len(cs.Changes))
	}

	del, err := DeleteServiceDomain(s, place("api"), "sd-a", now)
	if err != nil {
		t.Fatal(err)
	}
	if del.Kind != "domain.service.delete" || del.Target != "service-domain:sd-a" {
		t.Errorf("service delete = %+v", del)
	}
	if o := reg[del.Kind].Overlay(del); o.Short != "✗ domain api-a.up.railway.test" || o.Tone != "danger" {
		t.Errorf("Overlay = %+v", o)
	}
	if _, err := DeleteServiceDomain(s, place("api"), "nope", now); err == nil {
		t.Error("unknown service domain accepted")
	}

	cdel, err := DeleteCustomDomain(s, place("api"), "Shop.Example.com", now)
	if err != nil {
		t.Fatal(err)
	}
	if cdel.Kind != "domain.custom.delete" || cdel.Target != "custom-domain:shop.example.com" {
		t.Errorf("custom delete = %+v", cdel)
	}
	if o := reg[cdel.Kind].Overlay(cdel); o.Short != "✗ domain shop.example.com" {
		t.Errorf("Overlay = %+v", o)
	}
	if _, err := DeleteCustomDomain(s, place("api"), "other.example.com", now); err == nil {
		t.Error("absent custom domain accepted")
	}
	for _, c := range []changeset.Change{add, del, cdel} {
		if err := reg[c.Kind].Validate(s, c); err != nil {
			t.Errorf("%s Validate: %v", c.Kind, err)
		}
	}
}

func TestCustomAddIdempotent(t *testing.T) {
	s := domainStore(t)
	f := domainFake(t)
	env, _ := newEnv(t, f, s)
	c, err := AddCustomDomain(store.New(), place("api"), "shop.example.com", 8080, now)
	if err != nil {
		t.Fatal(err)
	}
	res, err := runDomain(t, env, c, apply.StepRecord{})
	if err != nil {
		t.Fatal(err)
	}
	wantDone(t, res, c)
	if m := domainMutations(f); len(m) != 0 {
		t.Errorf("mutations = %v", m)
	}

	// Absent: created once and confirmed; a second run creates nothing more.
	c2, err := AddCustomDomain(s, place("api"), "new.example.com", 0, now)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		res, err = runDomain(t, env, c2, apply.StepRecord{Status: apply.StatusFailed})
		if err != nil {
			t.Fatal(err)
		}
		wantDone(t, res, c2)
	}
	if m := domainMutations(f); !reflect.DeepEqual(m, []string{"CustomDomainCreate"}) {
		t.Errorf("mutations = %v", m)
	}
}

func TestCustomAddCreateFails(t *testing.T) {
	s := domainStore(t)
	f := domainFake(t)
	env, _ := newEnv(t, f, s)
	c, _ := AddCustomDomain(s, place("api"), "new.example.com", 0, now)
	f.FailNext("CustomDomainCreate", "boom")
	if _, err := runDomain(t, env, c, apply.StepRecord{}); err == nil {
		t.Error("failed create succeeded")
	}
	if got := domainMutations(f); len(got) != 0 {
		t.Errorf("mutations = %v", got)
	}
}

// blindDomains is an API whose Domains never lists anything.
type blindDomains struct{ apply.RailwayAPI }

func (blindDomains) Domains(context.Context, string, string, string) (railway.Domains, error) {
	return railway.Domains{}, nil
}

func TestDomainAddNotConfirmed(t *testing.T) {
	s := domainStore(t)
	f := newFake(t)
	env, _ := newEnv(t, f, s)
	env.API = blindDomains{env.API}
	c, _ := AddCustomDomain(s, place("api"), "new.example.com", 0, now)
	if _, err := runDomain(t, env, c, apply.StepRecord{}); err == nil || err.Error() != "not in Railway after create: new.example.com" {
		t.Errorf("custom add: err = %v", err)
	}
	sc := AddServiceDomain(place("api"), 0, now)
	if _, err := runDomain(t, env, sc, apply.StepRecord{}); err == nil || !strings.HasPrefix(err.Error(), "not in Railway after create: api-") {
		t.Errorf("service add: err = %v", err)
	}
}

func TestServiceAddRecordsIDAndResumes(t *testing.T) {
	s := domainStore(t)
	f := domainFake(t)
	env, _ := newEnv(t, f, s)
	c := AddServiceDomain(place("api"), 8080, now)
	key := ownDomainStep(t, c).Key()

	// The confirm read (the second Domains) fails after the create.
	env.API = &domainHooks{RailwayAPI: env.API, failRead: 2}
	if _, err := runDomain(t, env, c, apply.StepRecord{}); err == nil {
		t.Fatal("step succeeded with a failed confirm")
	}
	if m := domainMutations(f); !reflect.DeepEqual(m, []string{"ServiceDomainCreate"}) {
		t.Fatalf("mutations = %v", m)
	}
	prior := env.Journal.Step(key)
	var data struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(prior.Data, &data); err != nil || data.ID == "" {
		t.Fatalf("Data = %s (%v)", prior.Data, err)
	}
	if strings.Contains(string(prior.Data), "railway.test") {
		t.Errorf("Data holds more than ids: %s", prior.Data)
	}

	prior.Status = apply.StatusFailed
	env.API = env.API.(*domainHooks).RailwayAPI
	res, err := runDomain(t, env, c, prior)
	if err != nil {
		t.Fatal(err)
	}
	wantDone(t, res, c)
	if m := domainMutations(f); len(m) != 1 {
		t.Errorf("resume created again: %v", m)
	}

	// A rebuild acts on nothing and reads nothing.
	before := len(f.Mutations())
	res, err = runDomain(t, env, c, apply.StepRecord{Status: apply.StatusDone})
	if err != nil || len(res.Outcomes) != 0 || len(f.Mutations()) != before {
		t.Errorf("rebuild: res %+v err %v", res, err)
	}

	// A recorded id that is gone: created anew.
	gone, _ := json.Marshal(map[string]string{"id": "sd-gone"})
	if _, err := runDomain(t, env, c, apply.StepRecord{Status: apply.StatusFailed, Data: gone}); err != nil {
		t.Fatal(err)
	}
	if m := domainMutations(f); len(m) != 2 {
		t.Errorf("mutations = %v", m)
	}
}

func TestDeletesAbsentAreDone(t *testing.T) {
	s := domainStore(t)
	sdel, err := DeleteServiceDomain(s, place("api"), "sd-a", now)
	if err != nil {
		t.Fatal(err)
	}
	cdel, err := DeleteCustomDomain(s, place("api"), "shop.example.com", now)
	if err != nil {
		t.Fatal(err)
	}

	// Gone already: done, no mutation.
	f := newFake(t)
	env, _ := newEnv(t, f, s)
	for _, c := range []changeset.Change{sdel, cdel} {
		res, err := runDomain(t, env, c, apply.StepRecord{})
		if err != nil {
			t.Fatal(err)
		}
		wantDone(t, res, c)
	}
	if m := domainMutations(f); len(m) != 0 {
		t.Errorf("mutations = %v", m)
	}

	// Present: deleted by the id Railway reports now, and confirmed.
	f = domainFake(t)
	f.SetDomains("e1", "api", railway.Domains{
		Service: []railway.ServiceDomain{{ID: "sd-a", Domain: "api-a.up.railway.test"}},
		Custom:  []railway.CustomDomain{{ID: "cd-new", Domain: "Shop.Example.com"}},
	})
	env, _ = newEnv(t, f, s)
	for _, c := range []changeset.Change{sdel, cdel} {
		res, err := runDomain(t, env, c, apply.StepRecord{})
		if err != nil {
			t.Fatal(err)
		}
		wantDone(t, res, c)
	}
	if m := domainMutations(f); !reflect.DeepEqual(m, []string{"ServiceDomainDelete", "CustomDomainDelete"}) {
		t.Errorf("mutations = %v", m)
	}
	if r, _ := f.LastRequest("CustomDomainDelete"); r.Vars["id"] != "cd-new" {
		t.Errorf("custom delete id = %v", r.Vars["id"])
	}
}

func TestDomainCheck(t *testing.T) {
	s := domainStore(t)
	f := domainFake(t)
	env, _ := newEnv(t, f, s)
	reg := NewRegistry()
	have, _ := AddCustomDomain(store.New(), place("api"), "shop.example.com", 0, now)
	missing, _ := AddCustomDomain(s, place("api"), "new.example.com", 0, now)
	cdel, _ := DeleteCustomDomain(s, place("api"), "shop.example.com", now)
	sdel, _ := DeleteServiceDomain(s, place("api"), "sd-a", now)
	for _, tc := range []struct {
		c    changeset.Change
		want apply.Drift
	}{
		{have, apply.DriftAlreadyNew},
		{missing, apply.DriftNone},
		{cdel, apply.DriftNone},
		{sdel, apply.DriftNone},
		{AddServiceDomain(place("api"), 0, now), apply.DriftNone},
	} {
		d, err := reg[tc.c.Kind].Check(context.Background(), env, tc.c)
		if err != nil || d != tc.want {
			t.Errorf("%s %s: Check = %v, %v; want %v", tc.c.Kind, tc.c.Target, d, err, tc.want)
		}
	}
}

func TestDomainCorruptNewNeverMutates(t *testing.T) {
	s := domainStore(t)
	cadd, _ := AddCustomDomain(s, place("api"), "new.example.com", 0, now)
	cdel, _ := DeleteCustomDomain(s, place("api"), "shop.example.com", now)
	sdel, _ := DeleteServiceDomain(s, place("api"), "sd-a", now)
	sadd := AddServiceDomain(place("api"), 0, now)
	for _, tc := range []struct {
		c   changeset.Change
		bad []string
	}{
		{cadd, []string{`{"host":"New.example.com","port":0}`, `{`, `{"host":"new.example.com","port":0,"extra":1}`, `{"host":"new.example.com","port":0}x`, `{"host":"bad host","port":0}`, `{"host":"other.example.com","port":0}`, `null`}},
		{cdel, []string{`{"host":"Shop.example.com"}`, `{`, `{"host":"shop.example.com","x":1}`, `{"host":""}`, `{"host":"other.example.com"}`}},
		{sdel, []string{`{`, `{"id":"sd-a","domain":"a","x":1}`, `{"id":"","domain":"a"}`, `{"id":"sd-b","domain":"a"}`}},
		{sadd, []string{`{`, `{"port":"x"}`, `{"port":-1}`, `{"port":8080,"x":1}`, `{"port":1} {}`}},
	} {
		for _, bad := range tc.bad {
			f := domainFake(t)
			f.SetDomains("e1", "api", railway.Domains{Service: []railway.ServiceDomain{{ID: "sd-a"}}})
			env, _ := newEnv(t, f, s)
			c := tc.c
			c.New = json.RawMessage(bad)
			if _, err := runDomain(t, env, c, apply.StepRecord{}); err == nil {
				t.Errorf("%s %s: step succeeded", c.Kind, bad)
			}
			if _, err := NewRegistry()[c.Kind].Check(context.Background(), env, c); err == nil {
				t.Errorf("%s %s: Check succeeded", c.Kind, bad)
			}
			if m := domainMutations(f); len(m) != 0 {
				t.Errorf("%s %s: mutations %v", c.Kind, bad, m)
			}
		}
	}
}

func TestDomainStepsNotPatched(t *testing.T) {
	s := domainStore(t)
	reg := NewRegistry()
	dom, _ := AddCustomDomain(s, place("api"), "new.example.com", 8080, now)
	v := mustSet(t, s, place("api"), "PLAIN", "two")
	cs := changeset.ChangeSet{Changes: []changeset.Change{dom, v}}
	g, err := apply.Compile(cs, s, reg,
		func(envID string, changes []changeset.Change) apply.Step {
			return NewPatchStep(envID, changes, reg, "p1")
		}, NewRedeployStep)
	if err != nil {
		t.Fatal(err)
	}
	patch := g.Steps["patch:e1"]
	if patch == nil || !reflect.DeepEqual(patch.Serves(), []string{v.ID}) {
		t.Fatalf("patch step = %v", patch)
	}
	domKey := "domain:" + place("api").Key() + "/custom-domain:new.example.com"
	if st := g.Steps[domKey]; st == nil || !reflect.DeepEqual(st.Serves(), []string{dom.ID}) {
		t.Fatalf("domain step missing: %v", g.Order)
	}
	var patches, doms int
	for k := range g.Steps {
		switch {
		case strings.HasPrefix(k, "patch:"):
			patches++
		case strings.HasPrefix(k, "domain:"):
			doms++
		}
	}
	if patches != 1 || doms != 1 {
		t.Errorf("steps = %v", g.Order)
	}
}

// domainHooks wraps an API: failRead fails that Domains call (1-based),
// lose makes ServiceDomainCreate create and then report an error, noID
// makes it create and answer without an id.
type domainHooks struct {
	apply.RailwayAPI
	reads      int
	failRead   int
	lose, noID bool
}

func (h *domainHooks) Domains(ctx context.Context, projectID, envID, serviceID string) (railway.Domains, error) {
	h.reads++
	if h.reads == h.failRead {
		return railway.Domains{}, errors.New("read failed")
	}
	return h.RailwayAPI.Domains(ctx, projectID, envID, serviceID)
}

func (h *domainHooks) ServiceDomainCreate(ctx context.Context, envID, serviceID string, port int) (railway.ServiceDomain, error) {
	sd, err := h.RailwayAPI.ServiceDomainCreate(ctx, envID, serviceID, port)
	switch {
	case err != nil:
		return sd, err
	case h.lose:
		return railway.ServiceDomain{}, errors.New("connection reset")
	case h.noID:
		return railway.ServiceDomain{}, nil
	}
	return sd, nil
}

func journalData(t *testing.T, env *apply.Env, key string) serviceDomainData {
	t.Helper()
	var d serviceDomainData
	if err := json.Unmarshal(env.Journal.Step(key).Data, &d); err != nil {
		t.Fatalf("Data: %v", err)
	}
	return d
}

func TestServiceAddLostResponseAdopts(t *testing.T) {
	s := domainStore(t)
	f := domainFake(t)
	env, _ := newEnv(t, f, s)
	inner := env.API
	env.API = &domainHooks{RailwayAPI: inner, lose: true}
	c := AddServiceDomain(place("api"), 0, now)
	key := ownDomainStep(t, c).Key()
	if _, err := runDomain(t, env, c, apply.StepRecord{}); err == nil {
		t.Fatal("lost response succeeded")
	}
	d := journalData(t, env, key)
	if d.ID != "" || d.Before == nil || !reflect.DeepEqual(*d.Before, []string{"sd-a"}) {
		t.Fatalf("Data = %s", env.Journal.Step(key).Data)
	}

	env.API = inner
	prior := env.Journal.Step(key)
	prior.Status = apply.StatusFailed
	res, err := runDomain(t, env, c, prior)
	if err != nil {
		t.Fatal(err)
	}
	wantDone(t, res, c)
	if m := domainMutations(f); len(m) != 1 {
		t.Errorf("mutations = %v", m)
	}
	if d := journalData(t, env, key); d.ID == "" || d.ID == "sd-a" || d.Before != nil {
		t.Errorf("Data = %s", env.Journal.Step(key).Data)
	}
}

func TestServiceAddNoIDAdopts(t *testing.T) {
	s := domainStore(t)
	f := domainFake(t)
	env, _ := newEnv(t, f, s)
	env.API = &domainHooks{RailwayAPI: env.API, noID: true}
	c := AddServiceDomain(place("api"), 0, now)
	res, err := runDomain(t, env, c, apply.StepRecord{})
	if err != nil {
		t.Fatal(err)
	}
	wantDone(t, res, c)
	if d := journalData(t, env, ownDomainStep(t, c).Key()); d.ID == "" || d.ID == "sd-a" {
		t.Errorf("Data = %+v", d)
	}
}

func TestServiceAddBeforeCases(t *testing.T) {
	s := domainStore(t)
	c := AddServiceDomain(place("api"), 0, now)

	// Several ids not in before: a human looks, nothing is created.
	f := domainFake(t)
	env, _ := newEnv(t, f, s)
	f.SetDomains("e1", "api", railway.Domains{Service: []railway.ServiceDomain{{ID: "sd-a"}, {ID: "sd-b"}}})
	_, err := runDomain(t, env, c, apply.StepRecord{Status: apply.StatusFailed, Data: json.RawMessage(`{"before":[]}`)})
	if err == nil || err.Error() != "several new domains appeared · check the service" {
		t.Errorf("err = %v", err)
	}
	if m := domainMutations(f); len(m) != 0 {
		t.Errorf("mutations = %v", m)
	}

	// No new id: the earlier create never happened, create now.
	f = domainFake(t)
	env, _ = newEnv(t, f, s)
	if _, err := runDomain(t, env, c, apply.StepRecord{Status: apply.StatusFailed, Data: json.RawMessage(`{"before":["sd-a"]}`)}); err != nil {
		t.Fatal(err)
	}
	if m := domainMutations(f); len(m) != 1 {
		t.Errorf("mutations = %v", m)
	}
}

func TestServiceAddBadJournalData(t *testing.T) {
	s := domainStore(t)
	f := domainFake(t)
	env, _ := newEnv(t, f, s)
	c := AddServiceDomain(place("api"), 0, now)
	key := ownDomainStep(t, c).Key()
	for _, bad := range []string{`{"id":5}`, `{`, `[]`} {
		_, err := runDomain(t, env, c, apply.StepRecord{Status: apply.StatusFailed, Data: json.RawMessage(bad)})
		if err == nil || err.Error() != "bad journal data for "+key {
			t.Errorf("%s: err = %v", bad, err)
		}
	}
	if m := domainMutations(f); len(m) != 0 {
		t.Errorf("mutations = %v", m)
	}
}

func TestDomainDeleteCheckGone(t *testing.T) {
	s := domainStore(t)
	cdel, _ := DeleteCustomDomain(s, place("api"), "shop.example.com", now)
	sdel, _ := DeleteServiceDomain(s, place("api"), "sd-a", now)
	env, _ := newEnv(t, newFake(t), s)
	for _, c := range []changeset.Change{cdel, sdel} {
		if d, err := NewRegistry()[c.Kind].Check(context.Background(), env, c); err != nil || d != apply.DriftAlreadyNew {
			t.Errorf("%s: Check = %v, %v", c.Kind, d, err)
		}
	}
}

func TestServiceDeleteWithoutName(t *testing.T) {
	s := fixtureStore(t)
	s.SetDomains(place("api"), railway.Domains{Service: []railway.ServiceDomain{{ID: "sd-a"}}})
	c, err := DeleteServiceDomain(s, place("api"), "sd-a", now)
	if err != nil {
		t.Fatal(err)
	}
	if o := NewRegistry()[c.Kind].Overlay(c); o.Short != "✗ domain sd-a" {
		t.Errorf("Overlay = %+v", o)
	}
}
