package kinds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/monitor"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
)

const (
	kindLimits    = "limits.set"
	limitsTarget  = "limits"
	limitsPrefix  = "limits:"
	minLimitBytes = 256 << 20 // 256 MB (1024² units, as the UI writes MB)
)

// limits is a service's CPU/memory override, the New and Base.Value of a
// limits.set change. Both nil is no override.
type limits struct {
	CPU         *float64 `json:"cpu"`
	MemoryBytes *int64   `json:"memoryBytes"`
}

func (l limits) none() bool { return l.CPU == nil && l.MemoryBytes == nil }

func (l limits) equal(o limits) bool {
	return eqPtr(l.CPU, o.CPU) && eqPtr(l.MemoryBytes, o.MemoryBytes)
}

func eqPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func (l limits) marshal() []byte {
	raw, _ := json.Marshal(l)
	return raw
}

func decodeLimits(raw []byte) limits {
	var l limits
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &l)
	}
	return l
}

// strictDecodeLimits decodes a New that must be valid: a corrupt one would
// otherwise read as "no override" and clear the limits. It refuses unknown
// fields, trailing data, the literal null and a missing key: a clear is
// exactly {"cpu":null,"memoryBytes":null}.
func strictDecodeLimits(raw []byte) (limits, error) {
	var l limits
	if err := strictDecode(raw, &l); err != nil {
		return limits{}, errors.New("limits.set: bad change data")
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil || keys == nil || len(keys) != 2 {
		return limits{}, errors.New("limits.set: bad change data: cpu and memoryBytes are both required")
	}
	return l, nil
}

// validLimits is strictDecodeLimits plus checkLimits: the only way the apply
// path reads New, since a change loaded from disk or a frozen run was never
// validated. A half-null or out-of-range override is refused here.
func validLimits(c changeset.Change) (limits, error) {
	l, err := strictDecodeLimits(c.New)
	if err != nil {
		return limits{}, err
	}
	if err := checkLimits(c.Place, l); err != nil {
		return limits{}, fmt.Errorf("limits.set: bad change data: %w", err)
	}
	return l, nil
}

func limitsOf(cfg railway.EnvConfig, serviceID string) limits {
	sc := cfg.Services[serviceID]
	return limits{CPU: sc.CPU, MemoryBytes: sc.MemoryBytes}
}

func checkLimits(p domain.Place, l limits) error {
	if p.Shared() {
		return errors.New("limits belong to a service")
	}
	return checkLimitValues(l)
}

// checkLimitValues: both or neither, cpu above 0, memory at least 256 MB.
func checkLimitValues(l limits) error {
	if l.none() {
		return nil
	}
	if l.CPU == nil || l.MemoryBytes == nil {
		return errors.New("set both cpu and memory, or neither to clear the override")
	}
	if *l.CPU <= 0 {
		return errors.New("cpu must be above 0")
	}
	if *l.MemoryBytes < minLimitBytes {
		return errors.New("memory must be at least 256 MB")
	}
	return nil
}

// SetLimits makes a limits.set change: cpu and memoryBytes override the
// service's plan limits; both nil clears the override. Its base is the
// override the store's environment config holds now.
func SetLimits(s *store.Store, p domain.Place, cpu *float64, memoryBytes *int64, now time.Time) (changeset.Change, error) {
	nl := limits{CPU: cpu, MemoryBytes: memoryBytes}
	if err := checkLimits(p, nl); err != nil {
		return changeset.Change{}, err
	}
	cfg, ok := s.Config(p.EnvID)
	if !ok {
		return changeset.Change{}, errors.New("environment settings not loaded yet · r refresh")
	}
	return changeset.Change{
		ID: changeset.NewID(), Kind: kindLimits, Place: p, Target: limitsTarget,
		New: nl.marshal(), Base: changeset.Base{Value: string(limitsOf(cfg, p.ServiceID).marshal())},
		Created: now,
	}, nil
}

// Overlay and IsNoop below decode leniently: Check guards the apply path and
// Validate the UI path.

// limitsKind is limits.set: a service's CPU/memory override through the
// environment patch.
type limitsKind struct{}

func (limitsKind) Name() string { return kindLimits }

func (limitsKind) Validate(_ *store.Store, c changeset.Change) error {
	_, err := validLimits(c)
	return err
}

func (limitsKind) IsNoop(c changeset.Change) bool {
	return decodeLimits(c.New).equal(decodeLimits([]byte(c.Base.Value)))
}

func cpuText(v *float64) string {
	if v == nil {
		return "default"
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

// memText is in Railway's units (1024³ GB), as the panel shows the limits.
func memText(v *int64) string {
	if v == nil {
		return "default"
	}
	return monitor.FormatBytes(float64(*v))
}

func (limitsKind) Overlay(c changeset.Change) apply.Overlay {
	nl, bl := decodeLimits(c.New), decodeLimits([]byte(c.Base.Value))
	short := "limits → plan default"
	if !nl.none() {
		short = fmt.Sprintf("cpu %s → %s · mem %s → %s", cpuText(bl.CPU), cpuText(nl.CPU), memText(bl.MemoryBytes), memText(nl.MemoryBytes))
	}
	return apply.Overlay{Short: short, Long: short, Tone: "new"}
}

func (limitsKind) Secret(changeset.Change) bool { return false }

func (limitsKind) Writes(c changeset.Change) []string {
	return []string{limitsPrefix + c.Place.Key()}
}

func (limitsKind) Reads(changeset.Change) []string { return nil }

// Check reads the override from the environment config: the new one
// (clear: no override) is DriftAlreadyNew, the base DriftNone.
func (limitsKind) Check(ctx context.Context, env *apply.Env, c changeset.Change) (apply.Drift, error) {
	// Check runs before Patch on every apply path, and Validate does not:
	// a corrupt New must stop here, not become a clear.
	nl, err := validLimits(c)
	if err != nil {
		return 0, err
	}
	cfg, err := env.API.EnvironmentConfig(ctx, c.Place.EnvID)
	if err != nil {
		return 0, err
	}
	cur := limitsOf(cfg, c.Place.ServiceID)
	switch {
	case cur.equal(nl):
		return apply.DriftAlreadyNew, nil
	case cur.equal(decodeLimits([]byte(c.Base.Value))):
		return apply.DriftNone, nil
	}
	return apply.DriftChanged, nil
}

// Confirm reads the override back after a commit: Railway commits patches
// it ignores (a null limitOverride), so COMMITTED alone proves nothing.
func (k limitsKind) Confirm(ctx context.Context, env *apply.Env, c changeset.Change) (bool, error) {
	d, err := k.Check(ctx, env, c)
	return d == apply.DriftAlreadyNew, err
}

// Patch sets limitOverride.containers, or clears it with
// limitOverride {containers: null}: limitOverride null is silently ignored.
//
// Compile calls Patch before any Check, on data never validated: invalid
// data gives an empty fragment (ok stays true, so the change stays in the
// patch step, whose Check then refuses it before anything is sent).
func (limitsKind) Patch(c changeset.Change) (map[string]any, bool) {
	nl, err := validLimits(c)
	if err != nil {
		return map[string]any{}, true
	}
	var containers any
	if !nl.none() {
		containers = map[string]any{"cpu": *nl.CPU, "memoryBytes": *nl.MemoryBytes}
	}
	return map[string]any{"services": map[string]any{c.Place.ServiceID: map[string]any{
		"deploy": map[string]any{"limitOverride": map[string]any{"containers": containers}},
	}}}, true
}

// Redeploys is the service itself: a running container gets new limits on
// its next deploy.
func (limitsKind) Redeploys(_ *store.Store, c changeset.Change) []domain.Place {
	return []domain.Place{c.Place}
}

func (limitsKind) OwnSteps(changeset.Change) []apply.Step { return nil }

// PendingLimits is the override a limits.set change sets, for display: both
// nil clears it. ok is false for another kind or data that does not decode
// or is out of range.
func PendingLimits(c changeset.Change) (cpu *float64, memoryBytes *int64, ok bool) {
	if c.Kind != kindLimits {
		return nil, nil, false
	}
	l, err := strictDecodeLimits(c.New)
	if err != nil || checkLimitValues(l) != nil {
		return nil, nil, false
	}
	return l.CPU, l.MemoryBytes, true
}
