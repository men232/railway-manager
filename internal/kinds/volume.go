package kinds

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/store"
)

const (
	kindVolume   = "volume.size"
	volumeTarget = "volume:"
)

// volumeNew is the New of a volume.size change.
type volumeNew struct {
	VolumeID string `json:"volumeId"`
	SizeMB   int    `json:"sizeMB"`
}

// errUndecodable is strictDecode's only error. The decoder's own error can
// quote part of the data (an invalid character or escape), and a New may be
// a secret, so its text never leaves strictDecode.
var errUndecodable = errors.New("undecodable")

// strictDecode decodes one JSON value into v: no unknown fields, no trailing
// data.
func strictDecode(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errUndecodable
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errUndecodable
	}
	return nil
}

// decodeVolumeNew decodes strictly: a corrupt New must not become a resize.
func decodeVolumeNew(raw []byte) (volumeNew, error) {
	var v volumeNew
	if err := strictDecode(raw, &v); err != nil {
		return volumeNew{}, errors.New("volume.size: bad change data")
	}
	if v.VolumeID == "" || v.SizeMB <= 0 {
		return volumeNew{}, errors.New("volume.size: bad change data: volume id and a size above 0 are required")
	}
	return v, nil
}

// volumeNewLoose is for Patch, Writes, IsNoop and Overlay. Check decodes
// strictly and runs before Patch on every apply path (Validate guards only
// the UI path), so a bad New never reaches them there.
func volumeNewLoose(raw []byte) volumeNew {
	var v volumeNew
	_ = json.Unmarshal(raw, &v)
	return v
}

func baseMB(c changeset.Change) int {
	n, _ := strconv.Atoi(c.Base.Value)
	return n
}

// ResizeVolume makes a volume.size change: grow the volume mounted at p to
// sizeMB. A resize cannot be undone, so it only grows.
func ResizeVolume(s *store.Store, p domain.Place, volumeID string, sizeMB int, now time.Time) (changeset.Change, error) {
	if p.Shared() {
		return changeset.Change{}, errors.New("a volume belongs to a service")
	}
	mounted := false
	for _, m := range s.Volumes(p.EnvID, p.ServiceID) {
		if m.VolumeID == volumeID {
			mounted = true
		}
	}
	if !mounted {
		return changeset.Change{}, errors.New("volume is not mounted in this service")
	}
	cfg, ok := s.Config(p.EnvID)
	if !ok {
		return changeset.Change{}, errors.New("environment settings not loaded yet · r refresh")
	}
	vc, ok := cfg.Volumes[volumeID]
	if !ok {
		return changeset.Change{}, errors.New("unknown volume")
	}
	if sizeMB <= vc.SizeMB {
		return changeset.Change{}, errors.New("a volume can only grow")
	}
	raw, _ := json.Marshal(volumeNew{VolumeID: volumeID, SizeMB: sizeMB})
	return changeset.Change{
		ID: changeset.NewID(), Kind: kindVolume, Place: p, Target: volumeTarget + volumeID,
		New: raw, Base: changeset.Base{Value: strconv.Itoa(vc.SizeMB)}, Created: now,
	}, nil
}

// volumeKind is volume.size: a volume's size through the environment patch.
type volumeKind struct{}

func (volumeKind) Name() string { return kindVolume }

func (volumeKind) Validate(_ *store.Store, c changeset.Change) error {
	v, err := decodeVolumeNew(c.New)
	if err != nil {
		return err
	}
	if c.Place.Shared() {
		return errors.New("a volume belongs to a service")
	}
	if c.Target != volumeTarget+v.VolumeID {
		return errors.New("volume.size: target does not match the volume")
	}
	return nil
}

func (volumeKind) IsNoop(c changeset.Change) bool {
	return volumeNewLoose(c.New).SizeMB == baseMB(c)
}

func gbText(mb int) string { return fmt.Sprintf("%.1f GB", float64(mb)/1000) }

func (volumeKind) Overlay(c changeset.Change) apply.Overlay {
	s := fmt.Sprintf("disk %s → %s · can't be undone", gbText(baseMB(c)), gbText(volumeNewLoose(c.New).SizeMB))
	return apply.Overlay{Short: s, Long: s, Tone: "warn"}
}

func (volumeKind) Secret(changeset.Change) bool { return false }

func (volumeKind) Writes(c changeset.Change) []string {
	return []string{volumeTarget + c.Place.EnvID + "/" + volumeNewLoose(c.New).VolumeID}
}

func (volumeKind) Reads(changeset.Change) []string { return nil }

// Check reads the size from the environment config: at or above the new size
// is DriftAlreadyNew, the base size DriftNone.
//
// Validate does not run on this path, so Check also refuses a change whose
// volume is not in the environment or not mounted in the change's service:
// a change from disk must not resize another service's volume.
func (volumeKind) Check(ctx context.Context, env *apply.Env, c changeset.Change) (apply.Drift, error) {
	v, err := decodeVolumeNew(c.New)
	if err != nil {
		return 0, err
	}
	if c.Place.Shared() || c.Target != volumeTarget+v.VolumeID {
		return 0, errors.New("volume.size: bad change data: target does not match the volume")
	}
	cfg, err := env.API.EnvironmentConfig(ctx, c.Place.EnvID)
	if err != nil {
		return 0, err
	}
	vc, ok := cfg.Volumes[v.VolumeID]
	if !ok {
		return 0, errors.New("volume.size: the volume is not in this environment")
	}
	if err := checkMounted(ctx, env, c.Place, v.VolumeID); err != nil {
		return 0, err
	}
	cur := vc.SizeMB
	switch {
	case cur >= v.SizeMB:
		return apply.DriftAlreadyNew, nil
	case cur == baseMB(c):
		return apply.DriftNone, nil
	}
	return apply.DriftChanged, nil
}

// checkMounted reads the project's mounts: the volume must be mounted in the
// change's service. A partial read that shows the mount is enough.
func checkMounted(ctx context.Context, env *apply.Env, p domain.Place, volumeID string) error {
	mounts, err := env.API.ServiceVolumes(ctx, p.ProjectID)
	for _, m := range mounts {
		if m.EnvID == p.EnvID && m.VolumeID == volumeID {
			if m.ServiceID != p.ServiceID {
				return errors.New("volume.size: the volume is mounted in another service")
			}
			return nil
		}
	}
	if err != nil {
		return err
	}
	return errors.New("volume.size: the volume is not mounted in this service")
}

// Confirm reads the size back after a commit: COMMITTED alone proves nothing.
func (k volumeKind) Confirm(ctx context.Context, env *apply.Env, c changeset.Change) (bool, error) {
	d, err := k.Check(ctx, env, c)
	return d == apply.DriftAlreadyNew, err
}

func (volumeKind) Patch(c changeset.Change) (map[string]any, bool) {
	v := volumeNewLoose(c.New)
	return map[string]any{"volumes": map[string]any{v.VolumeID: map[string]any{"sizeMB": v.SizeMB}}}, true
}

// Redeploys is the service when the volume cannot be resized online.
func (volumeKind) Redeploys(s *store.Store, c changeset.Change) []domain.Place {
	cfg, ok := s.Config(c.Place.EnvID)
	if ok && cfg.Volumes[volumeNewLoose(c.New).VolumeID].AllowOnlineResize {
		return nil
	}
	return []domain.Place{c.Place}
}

func (volumeKind) OwnSteps(changeset.Change) []apply.Step { return nil }

// PendingVolumeSize is the volume and size (MB) a volume.size change sets,
// for display. ok is false for another kind or data that does not decode.
func PendingVolumeSize(c changeset.Change) (volumeID string, sizeMB int, ok bool) {
	if c.Kind != kindVolume {
		return "", 0, false
	}
	v, err := decodeVolumeNew(c.New)
	if err != nil {
		return "", 0, false
	}
	return v.VolumeID, v.SizeMB, true
}
