package kinds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/text"
)

const (
	kindSet    = "variable.set"
	kindDelete = "variable.delete"
	varPrefix  = "var:"
)

var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// newValue is the New of a variable.set change: the value, the location
// ids its references resolved to when the change was made, and the place it
// was copied from, if any. It is a secret.
type newValue struct {
	Value   string         `json:"value"`
	Refs    []string       `json:"refs,omitempty"`
	Copy    *copySource    `json:"copy,omitempty"`
	Planned *plannedSource `json:"planned,omitempty"`
}

// plannedSource is the pending change a set copied its value from: the
// value that change plans, not what Railway holds, so nothing is checked
// against Railway at apply. The link lets the change set take the copies
// with their source (CopiesOf) and keep them on its value (Follow).
type plannedSource struct {
	Loc    string `json:"loc"`
	Change string `json:"change"`
}

// copySource is the variable a set copied its value from: its location id
// and its store fingerprint when the change was made.
type copySource struct {
	Loc         string `json:"loc"`
	Fingerprint string `json:"fingerprint"`
}

// sourcePlace splits a copy source's location id into its place (without
// the project) and name; ok is false for a malformed id.
func (cs copySource) sourcePlace() (p domain.Place, name string, ok bool) {
	parts := strings.SplitN(cs.Loc, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return domain.Place{}, "", false
	}
	p.EnvID = parts[0]
	if parts[1] != "shared" {
		p.ServiceID = parts[1]
	}
	return p, parts[2], true
}

// CopySource is the location id a variable.set copied its value from, for
// screens that show it; ok is false for any other change.
func CopySource(c changeset.Change) (loc string, ok bool) {
	if c.Kind != kindSet {
		return "", false
	}
	nv := decodeNew(c)
	switch {
	case nv.Copy != nil:
		return nv.Copy.Loc, true
	case nv.Planned != nil:
		return nv.Planned.Loc, true
	}
	return "", false
}

// CopyPlanned makes a variable.set at p of the value src, a pending
// variable.set, plans: the copy holds that value and the link to src. A
// value with references is refused (it would resolve again at p).
func CopyPlanned(s *store.Store, pending []changeset.Change, p domain.Place, name string, src changeset.Change, now time.Time) (changeset.Change, error) {
	if src.Kind != kindSet {
		return changeset.Change{}, errors.New("copy source plans no value")
	}
	sv := decodeNew(src)
	if !domain.IsLiteral(sv.Value) {
		return changeset.Change{}, errors.New("copy source holds a reference")
	}
	c, err := SetVariable(s, pending, p, name, sv.Value, now)
	if err != nil {
		return changeset.Change{}, err
	}
	nv := decodeNew(c)
	nv.Planned = &plannedSource{Loc: locationOf(src), Change: src.ID}
	if c.New, err = json.Marshal(nv); err != nil {
		return changeset.Change{}, err
	}
	return c, nil
}

// CopiesOf are the pending changes that copy the planned value of a change
// in ids, and the copies of those, in change set order; ids themselves are
// left out.
func CopiesOf(pending []changeset.Change, ids ...string) []changeset.Change {
	in := map[string]bool{}
	for _, id := range ids {
		in[id] = true
	}
	var out []changeset.Change
	for grew := true; grew; {
		grew = false
		for _, c := range pending {
			if in[c.ID] || c.Kind != kindSet {
				continue
			}
			if pl := decodeNew(c).Planned; pl != nil && in[pl.Change] {
				in[c.ID], grew = true, true
				out = append(out, c)
			}
		}
	}
	return out
}

// Follow is copy c set to value, keeping its link: the copy follows its
// source to a new planned value.
func Follow(c changeset.Change, value string) (changeset.Change, error) {
	nv := decodeNew(c)
	nv.Value = value
	var err error
	c.New, err = json.Marshal(nv)
	return c, err
}

// SetValue is the value a variable.set writes (a copy's copied value), for
// a producer that gives other places the same new value; ok is false for
// any other change. It is a secret: never render it.
func SetValue(c changeset.Change) (value string, ok bool) {
	if c.Kind != kindSet {
		return "", false
	}
	return decodeNew(c).Value, true
}

func decodeNew(c changeset.Change) newValue {
	var nv newValue
	if len(c.New) > 0 {
		_ = json.Unmarshal(c.New, &nv)
	}
	return nv
}

// CheckName reports why name cannot be a Railway variable name, or nil.
func CheckName(name string) error { return checkName(name) }

func checkName(name string) error {
	if strings.HasPrefix(name, "RAILWAY_") {
		return fmt.Errorf("%s is managed by Railway", name)
	}
	if !namePattern.MatchString(name) {
		return fmt.Errorf("invalid variable name %q: use letters, digits and _, not starting with a digit", name)
	}
	return nil
}

// envName is the environment's own name, or its id when unknown.
func envName(s *store.Store, envID string) string {
	for _, p := range s.Projects() {
		for _, e := range p.Environments {
			if e.ID == envID {
				return e.Name
			}
		}
	}
	return envID
}

// resolveRefs resolves every reference in value, in order of first
// appearance, or names the first that does not resolve. A reference resolves
// to a variable in the store, or to a location known(loc) accepts (a pending
// variable.set, or a ref already resolved in a change's New).
func resolveRefs(s *store.Store, p domain.Place, value string, known func(loc string) bool) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, r := range domain.ParseReferences(value) {
		id := s.ResolveReference(p, r)
		if id == "" {
			svc := p.ServiceID
			switch {
			case r.Shared:
				svc = ""
			case r.Service != "":
				svc = s.ServiceIDByName(p.EnvID, r.Service)
			}
			loc := domain.LocationID(p.EnvID, svc, r.Name)
			switch {
			case (r.Shared || r.Service == "" || svc != "") && known(loc):
				id = loc
			case r.Shared:
				return nil, fmt.Errorf("shared has no %s", r.Name)
			case r.Service == "":
				return nil, fmt.Errorf("%s has no %s", s.ServiceName(p.ServiceID), r.Name)
			case svc == "":
				return nil, fmt.Errorf("no service %s in %s", r.Service, envName(s, p.EnvID))
			default:
				return nil, fmt.Errorf("%s has no %s", r.Service, r.Name)
			}
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// pendingSets are the locations pending variable.set changes add or change.
func pendingSets(pending []changeset.Change) func(loc string) bool {
	locs := map[string]bool{}
	for _, c := range pending {
		if c.Kind == kindSet {
			locs[locationOf(c)] = true
		}
	}
	return func(loc string) bool { return locs[loc] }
}

// directReferrers are the variables whose own value references locID,
// itself excluded.
func directReferrers(s *store.Store, locID string) []string {
	var out []string
	for _, id := range s.Dependents(locID) {
		v := s.Variable(id)
		if v == nil || id == locID {
			continue
		}
		for _, r := range domain.ParseReferences(v.Unrendered) {
			if s.ResolveReference(v.Place, r) == locID {
				out = append(out, id)
				break
			}
		}
	}
	return out
}

func checkUnreferenced(s *store.Store, p domain.Place, name string) error {
	if n := len(directReferrers(s, domain.LocationID(p.EnvID, p.ServiceID, name))); n > 0 {
		return fmt.Errorf("referenced by %d variables: delete or change them first", n)
	}
	return nil
}

// baseOf is the variable's state as the store holds it, without its value.
func baseOf(s *store.Store, p domain.Place, name string) changeset.Base {
	v := s.Variable(domain.LocationID(p.EnvID, p.ServiceID, name))
	if v == nil {
		return changeset.Base{Absent: true}
	}
	return changeset.Base{Sealed: v.Sealed, Fingerprint: v.Fingerprint}
}

// SetVariable makes a variable.set change, resolved against the store now:
// its base and the targets of the references in value. A
// reference may target a variable a pending variable.set adds;
// it is stored as that variable's location id.
func SetVariable(s *store.Store, pending []changeset.Change, p domain.Place, name, value string, now time.Time) (changeset.Change, error) {
	if err := checkName(name); err != nil {
		return changeset.Change{}, err
	}
	refs, err := resolveRefs(s, p, value, pendingSets(pending))
	if err != nil {
		return changeset.Change{}, err
	}
	raw, err := json.Marshal(newValue{Value: value, Refs: refs})
	if err != nil {
		return changeset.Change{}, err
	}
	return changeset.Change{
		ID: changeset.NewID(), Kind: kindSet, Place: p, Target: name,
		New: raw, Base: baseOf(s, p, name), Created: now,
	}, nil
}

// CopyVariable makes a variable.set of name at p holding the current
// unrendered value of srcName at src, recording the source and its
// fingerprint so a later change of the source is drift. A sealed, empty or
// missing source is refused.
func CopyVariable(s *store.Store, pending []changeset.Change, p domain.Place, name string, src domain.Place, srcName string, now time.Time) (changeset.Change, error) {
	loc := domain.LocationID(src.EnvID, src.ServiceID, srcName)
	v := s.Variable(loc)
	switch {
	case v == nil:
		return changeset.Change{}, errors.New("copy source not found")
	case v.Sealed:
		return changeset.Change{}, errors.New("copy source is sealed")
	case v.Unrendered == "":
		return changeset.Change{}, errors.New("copy source is empty")
	}
	c, err := SetVariable(s, pending, p, name, v.Unrendered, now)
	if err != nil {
		return changeset.Change{}, err
	}
	nv := decodeNew(c)
	nv.Copy = &copySource{Loc: loc, Fingerprint: v.Fingerprint}
	if c.New, err = json.Marshal(nv); err != nil {
		return changeset.Change{}, err
	}
	return c, nil
}

// DeleteVariable makes a variable.delete change. A variable other variables
// reference is refused.
func DeleteVariable(s *store.Store, p domain.Place, name string, now time.Time) (changeset.Change, error) {
	if err := checkName(name); err != nil {
		return changeset.Change{}, err
	}
	if err := checkUnreferenced(s, p, name); err != nil {
		return changeset.Change{}, err
	}
	return changeset.Change{
		ID: changeset.NewID(), Kind: kindDelete, Place: p, Target: name,
		Base: baseOf(s, p, name), Created: now,
	}, nil
}

// variableKind is variable.set, or variable.delete when del is set.
type variableKind struct{ del bool }

func (k variableKind) Name() string {
	if k.del {
		return kindDelete
	}
	return kindSet
}

func (k variableKind) Validate(s *store.Store, c changeset.Change) error {
	if err := checkName(c.Target); err != nil {
		return err
	}
	if k.del {
		return checkUnreferenced(s, c.Place, c.Target)
	}
	// A ref resolved when the change was made may target a pending change
	// the store does not hold yet.
	nv := decodeNew(c)
	_, err := resolveRefs(s, c.Place, nv.Value, func(loc string) bool { return slices.Contains(nv.Refs, loc) })
	return err
}

// ValidateData is Validate without the store: the name, and for a set a New
// that decodes strictly and holds a value — in a KEYFILE group, a value
// Mongo takes as a KEYFILE (CheckKeyFile). The references were resolved
// when the change was made; whether they still resolve is Validate's
// concern once the store is read.
func (k variableKind) ValidateData(c changeset.Change) error {
	if err := checkName(c.Target); err != nil {
		return err
	}
	if k.del {
		return nil
	}
	nv, err := validNew(c)
	if err == nil && keyFileGroup(c.Group) {
		// A KEYFILE write, whoever made it (an edit in Variables keeps
		// the group): Mongo must take the value.
		err = CheckKeyFile(nv.Value)
	}
	return err
}

// validNew decodes a variable.set New strictly: no unknown fields, no
// trailing data, and a value present. It is the only way the apply path
// (Check, Patch) reads New, since a change loaded from disk or a frozen run
// was never validated and a lenient decode would apply an empty value.
func validNew(c changeset.Change) (newValue, error) {
	var nv struct {
		Value   *string        `json:"value"`
		Refs    []string       `json:"refs"`
		Copy    *copySource    `json:"copy"`
		Planned *plannedSource `json:"planned"`
	}
	if err := strictDecode(c.New, &nv); err != nil {
		return newValue{}, errors.New("variable.set: bad change data")
	}
	if nv.Value == nil {
		return newValue{}, errors.New("variable.set: bad change data: no value")
	}
	if nv.Copy != nil {
		if _, _, ok := nv.Copy.sourcePlace(); !ok || nv.Copy.Fingerprint == "" {
			return newValue{}, errors.New("variable.set: bad change data: bad copy source")
		}
	}
	if nv.Planned != nil && (nv.Planned.Change == "" || nv.Planned.Loc == "") {
		return newValue{}, errors.New("variable.set: bad change data: bad planned copy")
	}
	return newValue{Value: *nv.Value, Refs: nv.Refs, Copy: nv.Copy, Planned: nv.Planned}, nil
}

var _ apply.DataValidator = variableKind{}

// fingerprint is the fingerprint value would have at the change's location,
// unsealed.
func fingerprint(c changeset.Change, value string) string {
	return domain.Fingerprint(c.Target, value, false, domain.IsLiteral(value), c.Place.EnvID, c.Place.ServiceID)
}

func (k variableKind) IsNoop(c changeset.Change) bool {
	if k.del {
		return c.Base.Absent
	}
	if c.Base.Absent || c.Base.Sealed {
		return false
	}
	return fingerprint(c, decodeNew(c).Value) == c.Base.Fingerprint
}

func (k variableKind) Overlay(c changeset.Change) apply.Overlay {
	var short, long, tone string
	switch {
	case k.del:
		short, tone = "✗ delete", "danger"
	case c.Base.Absent:
		short, tone = "+ added", "new"
	default:
		n := utf8.RuneCountInString(decodeNew(c).Value)
		short, long, tone = fmt.Sprintf("→ %d chars", n), fmt.Sprintf("→ new value (%d chars)", n), "new"
	}
	if long == "" {
		long = short
	}
	return apply.Overlay{Short: short, Long: long, Tone: tone}
}

func (k variableKind) Secret(changeset.Change) bool { return true }

// NewFingerprint is the fingerprint of the value a set gives its target, as
// the store will compute it once applied (a sealed target stays sealed); ""
// for a delete or bad data.
func (k variableKind) NewFingerprint(c changeset.Change) string {
	if k.del {
		return ""
	}
	nv, err := validNew(c)
	if err != nil {
		return ""
	}
	if c.Base.Sealed {
		return domain.Fingerprint(c.Target, nv.Value, true, false, c.Place.EnvID, c.Place.ServiceID)
	}
	return fingerprint(c, nv.Value)
}

func locationOf(c changeset.Change) string {
	return domain.LocationID(c.Place.EnvID, c.Place.ServiceID, c.Target)
}

func (k variableKind) Writes(c changeset.Change) []string {
	return []string{varPrefix + locationOf(c)}
}

func (k variableKind) Reads(c changeset.Change) []string {
	if k.del {
		return nil
	}
	var out []string
	for _, r := range decodeNew(c).Refs {
		out = append(out, varPrefix+r)
	}
	return out
}

// Check compares the variable's unrendered value in Railway with the base
// and the new value. A sealed value reads back empty: a sealed base that is
// still there is DriftNone, gone is DriftChanged for a set and
// DriftAlreadyNew for a delete.
func (k variableKind) Check(ctx context.Context, env *apply.Env, c changeset.Change) (apply.Drift, error) {
	// Check runs before Patch on every apply path, and Validate does not:
	// a corrupt New must stop here, not become an empty value.
	var nv newValue
	if !k.del {
		var err error
		if nv, err = validNew(c); err != nil {
			return 0, err
		}
	}
	d, err := k.checkTarget(ctx, env, c, nv)
	if err != nil || d != apply.DriftNone || nv.Copy == nil {
		return d, err
	}
	return checkCopySource(ctx, env, c, *nv.Copy)
}

// checkTarget is Check of the target alone, without a copy source.
func (k variableKind) checkTarget(ctx context.Context, env *apply.Env, c changeset.Change, nv newValue) (apply.Drift, error) {
	vars, err := env.ReadPlace(ctx, c.Place)
	if err != nil {
		return 0, err
	}
	cur, present := vars[c.Target]
	if c.Base.Sealed {
		// The value is unreadable; only presence is known.
		switch {
		case k.del && !present:
			return apply.DriftAlreadyNew, nil
		case !k.del && !present:
			return apply.DriftChanged, nil // deleted in Railway meanwhile
		}
		return apply.DriftNone, nil
	}
	if k.del {
		switch {
		case !present:
			return apply.DriftAlreadyNew, nil
		case !c.Base.Absent && fingerprint(c, cur) == c.Base.Fingerprint:
			return apply.DriftNone, nil
		}
		return apply.DriftChanged, nil
	}
	switch {
	case present && cur == nv.Value:
		return apply.DriftAlreadyNew, nil
	case c.Base.Absent && !present:
		return apply.DriftNone, nil
	case !c.Base.Absent && present && fingerprint(c, cur) == c.Base.Fingerprint:
		return apply.DriftNone, nil
	}
	return apply.DriftChanged, nil
}

// checkCopySource reads the copy source from Railway: DriftChanged when it
// is gone or its fingerprint is not the one recorded, else DriftNone.
func checkCopySource(ctx context.Context, env *apply.Env, c changeset.Change, cs copySource) (apply.Drift, error) {
	src, name, ok := cs.sourcePlace()
	if !ok {
		return 0, errors.New("variable.set: bad change data: bad copy source")
	}
	src.ProjectID = c.Place.ProjectID
	if env.Store != nil {
		if p := env.Store.PlaceOf(src.EnvID, src.ServiceID); p.ProjectID != "" {
			src.ProjectID = p.ProjectID
		}
	}
	vars, err := env.ReadPlace(ctx, src)
	if err != nil {
		return 0, err
	}
	cur, present := vars[name]
	if !present || domain.Fingerprint(name, cur, false, domain.IsLiteral(cur), src.EnvID, src.ServiceID) != cs.Fingerprint {
		return apply.DriftChanged, nil
	}
	return apply.DriftNone, nil
}

// SkipReason names the copy source of a drifted copy whose target still
// holds its base (read through env's cache, as Check read it). It is "" for
// a delete, a set without a copy source, and a copy whose target itself
// changed, which the default reason covers. Without env.Store the reason
// names no place.
func (k variableKind) SkipReason(ctx context.Context, env *apply.Env, c changeset.Change) string {
	if k.del {
		return ""
	}
	nv, err := validNew(c)
	if err != nil || nv.Copy == nil {
		return ""
	}
	if d, err := k.checkTarget(ctx, env, c, nv); err != nil || d != apply.DriftNone {
		return ""
	}
	src, _, ok := nv.Copy.sourcePlace()
	if env.Store == nil || !ok {
		return "copy source changed since the change was made"
	}
	at := text.Sanitize(env.Store.EnvName(src.EnvID) + " · " + env.Store.ServiceName(src.ServiceID))
	return "copy source " + at + " changed since the change was made"
}

// Drift reasons of variable changes, compared with the store.
const (
	DriftValueChanged = "value changed on Railway"
	DriftNowExists    = "variable now exists"
	DriftGone         = "variable gone"
	DriftCopySource   = "copy source changed"
)

// StoreDrift compares the change's base with the store as Check compares it
// with Railway: a target that holds the new value, or is gone for a delete,
// has not drifted; a sealed base is compared by presence only. The reason
// never holds a value. A place the store holds no variable at is taken as
// not read (the inventory skipped its environment or service, or read it
// partially), so a missing variable there is not reported gone; the cost is
// that the last variable of a place deleted on Railway is not flagged.
//
// A set with a copy source whose target has not drifted and does not hold
// the new value yet drifts when the source's fingerprint in the store is
// not the recorded one, or the source is gone from a place the store holds
// variables at.
func (k variableKind) StoreDrift(s *store.Store, c changeset.Change) string {
	if r := k.storeDriftBase(s, c); r != "" || k.del {
		return r
	}
	nv := decodeNew(c)
	if nv.Copy == nil {
		return ""
	}
	if v := s.Variable(locationOf(c)); v != nil && !v.Sealed && v.Unrendered == nv.Value {
		return ""
	}
	src, _, ok := nv.Copy.sourcePlace()
	if !ok {
		return ""
	}
	sv := s.Variable(nv.Copy.Loc)
	switch {
	case sv == nil && len(s.VariablesAt(src)) == 0:
		return "" // the source's place was not read
	case sv == nil || sv.Fingerprint != nv.Copy.Fingerprint:
		return DriftCopySource
	}
	return ""
}

func (k variableKind) storeDriftBase(s *store.Store, c changeset.Change) string {
	v := s.Variable(domain.LocationID(c.Place.EnvID, c.Place.ServiceID, c.Target))
	switch {
	case v == nil && len(s.VariablesAt(c.Place)) == 0:
		return ""
	case v == nil && k.del:
		return ""
	case v == nil && c.Base.Absent:
		return ""
	case v == nil:
		return DriftGone
	case c.Base.Absent:
		if !k.del && !v.Sealed && v.Unrendered == decodeNew(c).Value {
			return ""
		}
		return DriftNowExists
	case c.Base.Sealed:
		return ""
	case !k.del && !v.Sealed && v.Unrendered == decodeNew(c).Value:
		return ""
	case v.Fingerprint != c.Base.Fingerprint:
		return DriftValueChanged
	}
	return ""
}

var (
	_ apply.StoreDrifter     = variableKind{}
	_ apply.NewFingerprinter = variableKind{}
	_ apply.SkipReasoner     = variableKind{}
)

// Confirm reports whether Railway holds the change after a commit. A sealed
// set cannot be read back: its variable still being there is all that can
// be confirmed. Everything else is confirmed by Check.
func (k variableKind) Confirm(ctx context.Context, env *apply.Env, c changeset.Change) (bool, error) {
	if c.Base.Sealed && !k.del {
		vars, err := env.ReadPlace(ctx, c.Place)
		if err != nil {
			return false, err
		}
		_, present := vars[c.Target]
		return present, nil
	}
	d, err := k.Check(ctx, env, c)
	return d == apply.DriftAlreadyNew, err
}

// Patch sets the variable's value, or deletes it (null).
//
// Compile calls Patch before any Check, on data never validated: an invalid
// set gives an empty fragment (ok stays true, so the change stays in the
// patch step, whose Check then refuses it before anything is sent).
func (k variableKind) Patch(c changeset.Change) (map[string]any, bool) {
	var v any
	if !k.del {
		nv, err := validNew(c)
		if err != nil {
			return map[string]any{}, true
		}
		v = map[string]any{"value": nv.Value}
	}
	entry := map[string]any{c.Target: v}
	if c.Place.Shared() {
		return map[string]any{"sharedVariables": entry}, true
	}
	return map[string]any{"services": map[string]any{c.Place.ServiceID: map[string]any{"variables": entry}}}, true
}

// Redeploys are the change's own place (unless shared) and the place of
// every variable that references it, transitively, unique and in order. A
// database's own root password set in its password group redeploys no
// database place (dbOwnPassword).
func (k variableKind) Redeploys(s *store.Store, c changeset.Change) []domain.Place {
	var out []domain.Place
	seen := map[string]bool{}
	var dbPlaces map[string]bool
	if !k.del && dbOwnPassword(c) {
		dbPlaces = map[string]bool{c.Place.Key(): true}
		for _, id := range directReferrers(s, locationOf(c)) {
			if v := s.Variable(id); v != nil && v.Name == c.Target && v.Place.EnvID == c.Place.EnvID && storeMember(s, c, v.Place) {
				dbPlaces[v.Place.Key()] = true
			}
		}
	}
	add := func(p domain.Place) {
		if p.Shared() || seen[p.Key()] || dbPlaces[p.Key()] {
			return
		}
		seen[p.Key()] = true
		out = append(out, p)
	}
	add(s.PlaceOf(c.Place.EnvID, c.Place.ServiceID))
	for _, id := range s.Dependents(locationOf(c)) {
		if v := s.Variable(id); v != nil {
			add(s.PlaceOf(v.Place.EnvID, v.Place.ServiceID))
		}
	}
	return out
}

func (k variableKind) OwnSteps(changeset.Change) []apply.Step { return nil }

// memberVars are what makes a place referencing a Mongo root password a
// replica member, as rotation's database detection decides: replica nodes
// carry REPLICA_SET_NAME and a KEYFILE; init helpers and apps do not.
// Postgres and Redis have no members.
var memberVars = []string{"REPLICA_SET_NAME", "KEYFILE"}

// storeMember reports whether p, whose variable c.Target references the
// owner's, is a member of the database c sets the root password of.
func storeMember(s *store.Store, c changeset.Change, p domain.Place) bool {
	if c.Target != rotation.RootVar("mongo") {
		return false
	}
	for _, n := range memberVars {
		if s.Variable(domain.LocationID(p.EnvID, p.ServiceID, n)) == nil {
			return false
		}
	}
	return true
}

// dbOwnPassword reports whether c sets a database's own root password
// variable at its owner, in its password group. Mongo
// and Postgres read it only at first init, Redis already took the new
// password through CONFIG SET, and a database redeploy is a risk of its
// own: so the database's places (the owner and its replica members) do
// not redeploy for it. Apps that follow it by reference still do.
func dbOwnPassword(c changeset.Change) bool {
	dbKey, user, ok := rotation.ParseDBGroupKey(c.Group)
	if !ok || user == rotation.KeyFileUser || c.Kind != kindSet || c.Place.Key() != dbKey {
		return false
	}
	for _, kind := range []string{"mongo", "postgres", "redis"} {
		if rotation.RootVar(kind) == c.Target {
			return true
		}
	}
	return false
}

// serviceNamer resolves service names, e.g. apply.StoreView.
type serviceNamer interface {
	ServiceIDByName(envID, name string) string
}

// writtenLocations maps the location id of every variable a change writes
// ("var:<location>") to the ids of those changes.
func writtenLocations(reg apply.Registry, cs []changeset.Change) map[string][]string {
	out := map[string][]string{}
	for _, c := range cs {
		for _, w := range reg.Writes(c) {
			if loc, ok := strings.CutPrefix(w, varPrefix); ok {
				out[loc] = append(out[loc], c.ID)
			}
		}
	}
	return out
}

// freshReferrers reads the reference graph of fresh records of one
// environment and returns, per service id whose variables reference a
// written location directly or transitively, the ids of the changes that
// wrote those locations. Named references resolve only when names is not nil.
func freshReferrers(recs []railway.VarRecord, names serviceNamer, written map[string][]string) map[string]map[string]bool {
	rev := map[string][]string{} // location → locations referencing it
	svcOf := map[string]string{}
	for _, r := range recs {
		from := domain.LocationID(r.EnvID, r.ServiceID, r.Name)
		svcOf[from] = r.ServiceID
		for _, ref := range domain.ParseReferences(r.Unrendered) {
			svc := r.ServiceID
			switch {
			case ref.Shared:
				svc = ""
			case ref.Service != "":
				if names == nil {
					continue
				}
				if svc = names.ServiceIDByName(r.EnvID, ref.Service); svc == "" {
					continue
				}
			}
			to := domain.LocationID(r.EnvID, svc, ref.Name)
			rev[to] = append(rev[to], from)
		}
	}
	out := map[string]map[string]bool{}
	for loc, ids := range written {
		seen := map[string]bool{}
		stack := []string{loc}
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, from := range rev[cur] {
				if seen[from] {
					continue
				}
				seen[from] = true
				stack = append(stack, from)
				svc := svcOf[from]
				if svc == "" {
					continue
				}
				if out[svc] == nil {
					out[svc] = map[string]bool{}
				}
				for _, id := range ids {
					out[svc][id] = true
				}
			}
		}
	}
	return out
}
