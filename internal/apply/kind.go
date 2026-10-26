// Package apply turns a frozen change set into a DAG of steps, runs it and
// journals every step. Kinds and steps plug in through the interfaces here.
package apply

import (
	"context"
	"fmt"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/store"
)

// Overlay is how the effective view shows a change. Tone is "new",
// "danger", "warn" or "".
type Overlay struct {
	Short, Long, Tone string
}

// Drift is the result of a kind's check of its target against Railway.
type Drift int

const (
	// DriftNone: the target still holds the base (always for a sealed base).
	DriftNone Drift = iota
	// DriftAlreadyNew: the target holds the new value, or is gone for a delete.
	DriftAlreadyNew
	// DriftChanged: the target holds something else.
	DriftChanged
)

// Kind is one type of change: "variable.set", "service.restart", ...
type Kind interface {
	Name() string
	Validate(s *store.Store, c changeset.Change) error
	IsNoop(c changeset.Change) bool
	Overlay(c changeset.Change) Overlay
	Secret(c changeset.Change) bool
	// Writes and Reads are pure functions of the change data.
	Writes(c changeset.Change) []string
	Reads(c changeset.Change) []string
	// Check reads the target's current state through env and compares it
	// with c.Base and the new value.
	Check(ctx context.Context, env *Env, c changeset.Change) (Drift, error)
	// Patch is a fragment of the env's EnvironmentConfig patch, or ok=false.
	Patch(c changeset.Change) (fragment map[string]any, ok bool)
	// Redeploys lists the places to redeploy because of this change (empty
	// for redeploy/restart kinds themselves).
	Redeploys(s *store.Store, c changeset.Change) []domain.Place
	// OwnSteps are the steps besides the env patch (redeploy/restart kinds).
	OwnSteps(c changeset.Change) []Step
}

// DataValidator is implemented by a kind whose Validate also judges the
// change against the store (what its references resolve to, what refers to
// it). ValidateData judges the change's own data only, so a change loaded
// from disk can be checked before anything is read from Railway.
type DataValidator interface {
	ValidateData(c changeset.Change) error
}

// StoreDrifter is implemented by a kind whose base can be compared with
// what the store holds (a drift check without Railway). StoreDrift
// returns a short reason the target drifted from the base, or "" when it
// still matches it or already holds the new value. The reason never holds a
// value.
type StoreDrifter interface {
	StoreDrift(s *store.Store, c changeset.Change) string
}

// NewFingerprinter is implemented by a kind whose change gives its target a
// new value. NewFingerprint is that value's domain.Fingerprint, or "" when
// the change sets no value or its data is bad. It never returns the value.
type NewFingerprinter interface {
	NewFingerprint(c changeset.Change) string
}

// SkipReasoner is implemented by a kind whose drifted change may need a
// reason other than its target changing (a copy source that changed). The
// patch step skips a drifted change with SkipReason when it is not "". It
// may read through env, whose reads Check has cached. The reason never
// holds a value.
type SkipReasoner interface {
	SkipReason(ctx context.Context, env *Env, c changeset.Change) string
}

// Reopener is implemented by a kind whose steps act again while a change
// requiring theirs is not applied: Compile lists, in Graph.Reopen, the write
// steps serving the requiring changes under each write step that serves a
// change of a kind whose Reopen is true and whose own Reopenable is true
// (db.password: its dbpw step).
type Reopener interface {
	Reopen() bool
}

// Marker is implemented by a kind whose changes are decisions with no step
// (variable.keep): nothing serves them, and a run is done with them once
// the run finishes.
type Marker interface {
	Marker() bool
}

// Marker reports whether c's kind is a Marker.
func (r Registry) Marker(c changeset.Change) bool {
	m, ok := r[c.Kind].(Marker)
	return ok && m.Marker()
}

// Registry maps a change's Kind to its implementation. It is a
// changeset.Effects; unknown kinds have no writes or reads.
type Registry map[string]Kind

// Writes returns the targets the change writes.
func (r Registry) Writes(c changeset.Change) []string {
	if k, ok := r[c.Kind]; ok {
		return k.Writes(c)
	}
	return nil
}

// Reads returns the targets the change depends on.
func (r Registry) Reads(c changeset.Change) []string {
	if k, ok := r[c.Kind]; ok {
		return k.Reads(c)
	}
	return nil
}

// CheckData reports why a change cannot be applied as it stands, judged on
// its own data: a kind the registry does not know, or data its kind refuses.
// A kind without ValidateData is judged by Validate against an empty store,
// so CheckData needs nothing read from Railway.
func (r Registry) CheckData(c changeset.Change) error {
	k, ok := r[c.Kind]
	if !ok {
		return fmt.Errorf("unknown change kind %s", c.Kind)
	}
	if dv, ok := k.(DataValidator); ok {
		return dv.ValidateData(c)
	}
	return k.Validate(store.New(), c)
}

// StoreDrift is the kind's StoreDrift, or "" for a kind without one or one
// the registry does not know.
func (r Registry) StoreDrift(s *store.Store, c changeset.Change) string {
	if d, ok := r[c.Kind].(StoreDrifter); ok {
		return d.StoreDrift(s, c)
	}
	return ""
}

// NewFingerprint is the kind's NewFingerprint, or "" for a kind without one
// or one the registry does not know.
func (r Registry) NewFingerprint(c changeset.Change) string {
	if f, ok := r[c.Kind].(NewFingerprinter); ok {
		return f.NewFingerprint(c)
	}
	return ""
}

var _ changeset.Effects = Registry(nil)
