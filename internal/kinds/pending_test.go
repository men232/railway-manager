package kinds

import (
	"testing"

	"github.com/men232/railway-manager/internal/changeset"
)

func TestPendingLimits(t *testing.T) {
	cpu, mem, ok := PendingLimits(changeset.Change{Kind: kindLimits, New: []byte(`{"cpu":2,"memoryBytes":2000000000}`)})
	if !ok || cpu == nil || *cpu != 2 || mem == nil || *mem != 2e9 {
		t.Errorf("set: %v %v %v", cpu, mem, ok)
	}
	cpu, mem, ok = PendingLimits(changeset.Change{Kind: kindLimits, New: []byte(`{"cpu":null,"memoryBytes":null}`)})
	if !ok || cpu != nil || mem != nil {
		t.Errorf("clear: %v %v %v", cpu, mem, ok)
	}
	for _, c := range []changeset.Change{
		{Kind: kindLimits, New: []byte(`{"cpu":2}`)},
		{Kind: kindLimits, New: []byte(`{"cpu":2,"memoryBytes":null}`)},
		{Kind: kindLimits, New: []byte(`garbage`)},
		{Kind: kindVolume, New: []byte(`{"cpu":2,"memoryBytes":2}`)},
	} {
		if _, _, ok := PendingLimits(c); ok {
			t.Errorf("%s %s: ok", c.Kind, c.New)
		}
	}
}

func TestPendingVolumeSize(t *testing.T) {
	id, mb, ok := PendingVolumeSize(changeset.Change{Kind: kindVolume, New: []byte(`{"volumeId":"v1","sizeMB":10000}`)})
	if !ok || id != "v1" || mb != 10000 {
		t.Errorf("= %q %d %v", id, mb, ok)
	}
	for _, c := range []changeset.Change{
		{Kind: kindVolume, New: []byte(`{"volumeId":"v1"}`)},
		{Kind: kindLimits, New: []byte(`{"volumeId":"v1","sizeMB":1}`)},
	} {
		if _, _, ok := PendingVolumeSize(c); ok {
			t.Errorf("%s %s: ok", c.Kind, c.New)
		}
	}
}
