package kinds

import (
	"context"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/store"
)

const kindKeep = "variable.keep"

// Keep makes a variable.keep change: the place's variable is kept as it is
// while its password rotates (`k`). It writes nothing,
// so it is the one decision allowed in a read-only environment.
func Keep(p domain.Place, name, group string, now time.Time) changeset.Change {
	return changeset.Change{ID: changeset.NewID(), Kind: kindKeep, Place: p, Target: name, Group: group, Created: now}
}

// keepKind is variable.keep, a marker: no step, no write, no redeploy.
type keepKind struct{}

func (keepKind) Name() string { return kindKeep }

func (keepKind) Validate(_ *store.Store, c changeset.Change) error { return checkName(c.Target) }
func (keepKind) IsNoop(changeset.Change) bool                      { return false }
func (keepKind) Secret(changeset.Change) bool                      { return false }
func (keepKind) Marker() bool                                      { return true }

func (keepKind) Overlay(changeset.Change) apply.Overlay {
	s := "kept · old password stops working"
	return apply.Overlay{Short: s, Long: s, Tone: "danger"}
}

func (keepKind) Writes(changeset.Change) []string { return nil }
func (keepKind) Reads(changeset.Change) []string  { return nil }
func (keepKind) Check(context.Context, *apply.Env, changeset.Change) (apply.Drift, error) {
	return apply.DriftNone, nil
}
func (keepKind) Patch(changeset.Change) (map[string]any, bool)           { return nil, false }
func (keepKind) Redeploys(*store.Store, changeset.Change) []domain.Place { return nil }
func (keepKind) OwnSteps(changeset.Change) []apply.Step                  { return nil }

var _ apply.Marker = keepKind{}
