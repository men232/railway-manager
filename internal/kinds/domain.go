package kinds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/text"
)

// Domains cannot go through the environment patch (a patch with domains
// commits and is ignored), so each domain change is its own step that checks
// Railway before it acts and reads back after.
const (
	kindServiceDomainAdd = "domain.service.add"
	kindServiceDomainDel = "domain.service.delete"
	kindCustomDomainAdd  = "domain.custom.add"
	kindCustomDomainDel  = "domain.custom.delete"

	serviceDomainTarget = "service-domain:"
	customDomainTarget  = "custom-domain:"
	serviceDomainNew    = serviceDomainTarget + "new"
	domainStepPrefix    = "domain:"
)

var hostRE = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// ValidHost reports whether host is a domain name of two or more labels,
// letters, digits and inner hyphens only, no scheme, path or trailing dot.
func ValidHost(host string) bool { return hostRE.MatchString(host) }

// The New of each domain kind. Steps and Check decode it strictly.
type (
	serviceAddNew struct {
		Port int `json:"port"`
	}
	serviceDelNew struct {
		ID     string `json:"id"`
		Domain string `json:"domain"` // for the overlay only
	}
	customAddNew struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	customDelNew struct {
		Host string `json:"host"`
	}
)

// domainSpec is a decoded domain change: host (custom), id (service delete),
// port (adds).
type domainSpec struct {
	host, id string
	port     int
}

func validPort(p int) bool { return p >= 0 && p <= 65535 }

func hasHost(d railway.Domains, host string) (string, bool) {
	for _, c := range d.Custom {
		if strings.EqualFold(c.Domain, host) {
			return c.ID, true
		}
	}
	return "", false
}

func hasServiceDomain(d railway.Domains, id string) bool {
	for _, s := range d.Service {
		if s.ID == id {
			return true
		}
	}
	return false
}

func marshalNew(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}

func checkDomainPlace(p domain.Place) error {
	if p.Shared() {
		return errors.New("a domain belongs to a service")
	}
	return nil
}

// AddServiceDomain makes a domain.service.add change: a Railway-provided
// domain for the service at p, routed to port (0: Railway's default). Its
// target is the same for every add at p, so at most one is pending there.
func AddServiceDomain(p domain.Place, port int, now time.Time) changeset.Change {
	return changeset.Change{
		ID: changeset.NewID(), Kind: kindServiceDomainAdd, Place: p, Target: serviceDomainNew,
		New: marshalNew(serviceAddNew{Port: port}), Base: changeset.Base{Absent: true}, Created: now,
	}
}

// DeleteServiceDomain makes a domain.service.delete change for the Railway
// domain id the store lists at p.
func DeleteServiceDomain(s *store.Store, p domain.Place, id string, now time.Time) (changeset.Change, error) {
	if err := checkDomainPlace(p); err != nil {
		return changeset.Change{}, err
	}
	d, _ := s.Domains(p)
	for _, sd := range d.Service {
		if sd.ID == id && id != "" {
			return changeset.Change{
				ID: changeset.NewID(), Kind: kindServiceDomainDel, Place: p, Target: serviceDomainTarget + id,
				New: marshalNew(serviceDelNew{ID: id, Domain: sd.Domain}), Created: now,
			}, nil
		}
	}
	return changeset.Change{}, errors.New("no such railway domain on this service")
}

// AddCustomDomain makes a domain.custom.add change: host on the service at
// p, routed to port (0: Railway's default). It refuses a host the store
// already lists on the service.
func AddCustomDomain(s *store.Store, p domain.Place, host string, port int, now time.Time) (changeset.Change, error) {
	if err := checkDomainPlace(p); err != nil {
		return changeset.Change{}, err
	}
	if !ValidHost(host) {
		return changeset.Change{}, errors.New("not a valid host name")
	}
	if !validPort(port) {
		return changeset.Change{}, errors.New("port must be 1–65535, or empty for the default")
	}
	if d, ok := s.Domains(p); ok {
		if _, ok := hasHost(d, host); ok {
			return changeset.Change{}, errors.New("this service already has that domain")
		}
	}
	host = strings.ToLower(host)
	return changeset.Change{
		ID: changeset.NewID(), Kind: kindCustomDomainAdd, Place: p, Target: customDomainTarget + host,
		New: marshalNew(customAddNew{Host: host, Port: port}), Base: changeset.Base{Absent: true}, Created: now,
	}, nil
}

// DeleteCustomDomain makes a domain.custom.delete change for a host the
// store lists on the service at p. The id is looked up when the step runs.
func DeleteCustomDomain(s *store.Store, p domain.Place, host string, now time.Time) (changeset.Change, error) {
	if err := checkDomainPlace(p); err != nil {
		return changeset.Change{}, err
	}
	d, _ := s.Domains(p)
	if _, ok := hasHost(d, host); !ok || !ValidHost(host) {
		return changeset.Change{}, errors.New("no such custom domain on this service")
	}
	host = strings.ToLower(host)
	return changeset.Change{
		ID: changeset.NewID(), Kind: kindCustomDomainDel, Place: p, Target: customDomainTarget + host,
		New: marshalNew(customDelNew{Host: host}), Created: now,
	}, nil
}

// domainKind is one of the four domain kinds.
type domainKind struct{ name string }

func (k domainKind) Name() string { return k.name }

// decode decodes c.New strictly and checks it against c: a corrupt New never
// becomes a create or a delete.
func (k domainKind) decode(c changeset.Change) (domainSpec, error) {
	bad := func(err error) (domainSpec, error) {
		return domainSpec{}, fmt.Errorf("%s: bad change data: %w", k.name, err)
	}
	// The decoder's error is never shown: it can quote the data.
	undecodable := fmt.Errorf("%s: bad change data", k.name)
	if err := checkDomainPlace(c.Place); err != nil {
		return domainSpec{}, err
	}
	var sp domainSpec
	var target string
	switch k.name {
	case kindServiceDomainAdd:
		var n serviceAddNew
		if err := strictDecode(c.New, &n); err != nil {
			return domainSpec{}, undecodable
		}
		sp, target = domainSpec{port: n.Port}, serviceDomainNew
	case kindServiceDomainDel:
		var n serviceDelNew
		if err := strictDecode(c.New, &n); err != nil {
			return domainSpec{}, undecodable
		}
		if n.ID == "" {
			return bad(errors.New("a domain id is required"))
		}
		sp, target = domainSpec{id: n.ID, host: n.Domain}, serviceDomainTarget+n.ID
	case kindCustomDomainAdd:
		var n customAddNew
		if err := strictDecode(c.New, &n); err != nil {
			return domainSpec{}, undecodable
		}
		sp, target = domainSpec{host: n.Host, port: n.Port}, customDomainTarget+strings.ToLower(n.Host)
	case kindCustomDomainDel:
		var n customDelNew
		if err := strictDecode(c.New, &n); err != nil {
			return domainSpec{}, undecodable
		}
		sp, target = domainSpec{host: n.Host}, customDomainTarget+strings.ToLower(n.Host)
	default:
		return domainSpec{}, fmt.Errorf("unknown domain kind %s", k.name)
	}
	if k.custom() && !ValidHost(sp.host) {
		return bad(errors.New("not a valid host name"))
	}
	if k.custom() && sp.host != strings.ToLower(sp.host) {
		return bad(errors.New("host must be lowercase"))
	}
	if k.name == kindServiceDomainDel && sp.host == "" {
		sp.host = sp.id
	}
	if !validPort(sp.port) {
		return bad(errors.New("port out of range"))
	}
	if c.Target != target {
		return bad(errors.New("target does not match the domain"))
	}
	return sp, nil
}

func (k domainKind) custom() bool {
	return k.name == kindCustomDomainAdd || k.name == kindCustomDomainDel
}

func (k domainKind) add() bool {
	return k.name == kindCustomDomainAdd || k.name == kindServiceDomainAdd
}

func (k domainKind) Validate(_ *store.Store, c changeset.Change) error {
	_, err := k.decode(c)
	return err
}

func (domainKind) IsNoop(changeset.Change) bool { return false }
func (domainKind) Secret(changeset.Change) bool { return false }

// looseDomain is for Overlay and Label, which never act on it.
type looseDomain struct {
	Host   string `json:"host"`
	Domain string `json:"domain"`
	Port   int    `json:"port"`
}

func (k domainKind) Overlay(c changeset.Change) apply.Overlay {
	var n looseDomain
	_ = json.Unmarshal(c.New, &n)
	port := ""
	if n.Port != 0 {
		port = " → :" + strconv.Itoa(n.Port)
	}
	var s, tone string
	switch k.name {
	case kindServiceDomainAdd:
		s, tone = "+ railway domain"+port, "new"
	case kindCustomDomainAdd:
		s, tone = "+ domain "+text.Sanitize(n.Host)+port, "new"
	case kindServiceDomainDel:
		var d serviceDelNew
		_ = json.Unmarshal(c.New, &d)
		name := d.Domain
		if name == "" {
			name = d.ID
		}
		s, tone = "✗ domain "+text.Sanitize(name), "danger"
	default:
		s, tone = "✗ domain "+text.Sanitize(n.Host), "danger"
	}
	return apply.Overlay{Short: s, Long: s, Tone: tone}
}

func domainKey(c changeset.Change) string {
	return domainStepPrefix + c.Place.Key() + "/" + c.Target
}

func (domainKind) Writes(c changeset.Change) []string { return []string{domainKey(c)} }
func (domainKind) Reads(changeset.Change) []string    { return nil }

func (domainKind) Patch(changeset.Change) (map[string]any, bool) { return nil, false }

func (domainKind) Redeploys(*store.Store, changeset.Change) []domain.Place { return nil }

// Check reads the service's domains: a custom host already there for an
// add, or a domain already gone for a delete, is DriftAlreadyNew. A new
// Railway domain has nothing to compare with: DriftNone.
func (k domainKind) Check(ctx context.Context, env *apply.Env, c changeset.Change) (apply.Drift, error) {
	sp, err := k.decode(c)
	if err != nil {
		return 0, err
	}
	if k.name == kindServiceDomainAdd {
		return apply.DriftNone, nil
	}
	d, err := env.API.Domains(ctx, c.Place.ProjectID, c.Place.EnvID, c.Place.ServiceID)
	if err != nil {
		return 0, err
	}
	var there bool
	if k.custom() {
		_, there = hasHost(d, sp.host)
	} else {
		there = hasServiceDomain(d, sp.id)
	}
	if there == k.add() {
		return apply.DriftAlreadyNew, nil
	}
	return apply.DriftNone, nil
}

func (k domainKind) OwnSteps(c changeset.Change) []apply.Step {
	return []apply.Step{&domainStep{kind: k, c: c}}
}

// domainStep runs one domain change: check, act, read back.
type domainStep struct {
	kind domainKind
	c    changeset.Change
}

func (s *domainStep) Key() string      { return domainKey(s.c) }
func (s *domainStep) Deps() []string   { return nil }
func (s *domainStep) Serves() []string { return []string{s.c.ID} }
func (s *domainStep) Lock() string     { return s.c.Place.Key() }
func (s *domainStep) Label() string {
	return text.Sanitize(s.kind.Overlay(s.c).Short) + " at " + s.c.Place.Key()
}

// serviceDomainData is the step's journal data, ids only. Before is the
// service's Railway domain ids read just before a create, so a retry after a
// lost create response can adopt the one new id instead of creating a second
// domain; ID is the domain the step created or adopted.
type serviceDomainData struct {
	ID     string    `json:"id,omitempty"`
	Before *[]string `json:"before,omitempty"`
}

func serviceDomainIDs(d railway.Domains) []string {
	ids := make([]string, 0, len(d.Service))
	for _, sd := range d.Service {
		ids = append(ids, sd.ID)
	}
	return ids
}

// newServiceDomain returns the domain of d whose id is not in before: ok is
// false when there is none, an error when there are several.
func newServiceDomain(d railway.Domains, before []string) (railway.ServiceDomain, bool, error) {
	var found []railway.ServiceDomain
	for _, sd := range d.Service {
		if !slices.Contains(before, sd.ID) {
			found = append(found, sd)
		}
	}
	switch len(found) {
	case 0:
		return railway.ServiceDomain{}, false, nil
	case 1:
		return found[0], true, nil
	}
	return railway.ServiceDomain{}, false, errors.New("several new domains appeared · check the service")
}

// addServiceDomain creates a Railway domain at most once across retries: an
// id journaled earlier and still listed is done; with only the ids from
// before an earlier create, exactly one new id is adopted; otherwise it
// records the current ids, creates and journals the new id before
// confirming.
func (s *domainStep) addServiceDomain(ctx context.Context, env *apply.Env, prior apply.StepRecord, port int) (apply.Result, error) {
	var data serviceDomainData
	if len(prior.Data) > 0 {
		if err := json.Unmarshal(prior.Data, &data); err != nil {
			return apply.Result{}, fmt.Errorf("bad journal data for %s", s.Key())
		}
	}
	d, err := s.domains(ctx, env)
	if err != nil {
		return apply.Result{}, err
	}
	if data.ID != "" && hasServiceDomain(d, data.ID) {
		return s.done(), nil
	}
	if data.ID == "" && data.Before != nil {
		sd, ok, err := newServiceDomain(d, *data.Before)
		if err != nil {
			return apply.Result{}, err
		}
		if ok {
			if err := s.saveData(env, serviceDomainData{ID: sd.ID}); err != nil {
				return apply.Result{}, err
			}
			return s.done(), nil
		}
	}

	before := serviceDomainIDs(d)
	if err := s.saveData(env, serviceDomainData{Before: &before}); err != nil {
		return apply.Result{}, err
	}
	p := s.c.Place
	created, err := env.API.ServiceDomainCreate(ctx, p.EnvID, p.ServiceID, port)
	if err != nil {
		return apply.Result{}, err
	}
	if created.ID != "" {
		if err := s.saveData(env, serviceDomainData{ID: created.ID}); err != nil {
			return apply.Result{}, err
		}
	}
	if d, err = s.domains(ctx, env); err != nil {
		return apply.Result{}, err
	}
	if created.ID == "" {
		// Railway created a domain but returned no id: adopt the new one.
		sd, ok, err := newServiceDomain(d, before)
		if err != nil {
			return apply.Result{}, err
		}
		if !ok {
			return apply.Result{}, notInRailway("create", "railway domain")
		}
		if err := s.saveData(env, serviceDomainData{ID: sd.ID}); err != nil {
			return apply.Result{}, err
		}
		return s.done(), nil
	}
	if !hasServiceDomain(d, created.ID) {
		return apply.Result{}, notInRailway("create", created.Domain)
	}
	return s.done(), nil
}

func (s *domainStep) done() apply.Result {
	return apply.Result{Outcomes: map[string]apply.Outcome{s.c.ID: {Status: apply.StatusDone}}}
}

func (s *domainStep) domains(ctx context.Context, env *apply.Env) (railway.Domains, error) {
	p := s.c.Place
	return env.API.Domains(ctx, p.ProjectID, p.EnvID, p.ServiceID)
}

func notInRailway(action, host string) error {
	return fmt.Errorf("not in Railway after %s: %s", action, text.Sanitize(host))
}

func stillInRailway(host string) error {
	return fmt.Errorf("still in Railway after delete: %s", text.Sanitize(host))
}

// Run checks Railway before it acts, so a retry never acts twice. A run with
// prior.Status done is a rebuild: the step adds no steps, so it returns at
// once.
func (s *domainStep) Run(ctx context.Context, env *apply.Env, prior apply.StepRecord) (apply.Result, error) {
	if prior.Status == apply.StatusDone {
		return apply.Result{}, nil
	}
	sp, err := s.kind.decode(s.c)
	if err != nil {
		return apply.Result{}, err
	}
	p := s.c.Place
	switch s.kind.name {
	case kindCustomDomainAdd:
		d, err := s.domains(ctx, env)
		if err != nil {
			return apply.Result{}, err
		}
		if _, ok := hasHost(d, sp.host); ok {
			return s.done(), nil
		}
		if _, err := env.API.CustomDomainCreate(ctx, p.ProjectID, p.EnvID, p.ServiceID, sp.host, sp.port); err != nil {
			return apply.Result{}, err
		}
		if d, err = s.domains(ctx, env); err != nil {
			return apply.Result{}, err
		}
		if _, ok := hasHost(d, sp.host); !ok {
			return apply.Result{}, notInRailway("create", sp.host)
		}
	case kindCustomDomainDel:
		d, err := s.domains(ctx, env)
		if err != nil {
			return apply.Result{}, err
		}
		id, ok := hasHost(d, sp.host)
		if !ok {
			return s.done(), nil
		}
		if err := env.API.CustomDomainDelete(ctx, p.EnvID, id); err != nil {
			return apply.Result{}, err
		}
		if d, err = s.domains(ctx, env); err != nil {
			return apply.Result{}, err
		}
		if _, ok := hasHost(d, sp.host); ok {
			return apply.Result{}, stillInRailway(sp.host)
		}
	case kindServiceDomainAdd:
		return s.addServiceDomain(ctx, env, prior, sp.port)
	case kindServiceDomainDel:
		d, err := s.domains(ctx, env)
		if err != nil {
			return apply.Result{}, err
		}
		if !hasServiceDomain(d, sp.id) {
			return s.done(), nil
		}
		if err := env.API.ServiceDomainDelete(ctx, p.EnvID, sp.id); err != nil {
			return apply.Result{}, err
		}
		if d, err = s.domains(ctx, env); err != nil {
			return apply.Result{}, err
		}
		if hasServiceDomain(d, sp.id) {
			return apply.Result{}, stillInRailway(sp.host)
		}
	}
	return s.done(), nil
}

// saveData journals the created id before the step confirms it. Set
// replaces the whole record, so only Data changes.
func (s *domainStep) saveData(env *apply.Env, d serviceDomainData) error {
	if env.Journal == nil {
		return nil
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	rec := env.Journal.Step(s.Key())
	rec.Data = raw
	return env.Journal.Set(s.Key(), rec)
}
