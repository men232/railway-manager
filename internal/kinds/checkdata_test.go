package kinds

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/men232/railway-manager/internal/changeset"
)

// CheckData judges a change loaded from disk on its own data: before anything
// is read from Railway the store is empty, so a valid change that references
// another service must still pass.
func TestCheckDataLoaded(t *testing.T) {
	st := fixtureStore(t)
	reg := NewRegistry()
	limits := mustLimits(t, limitsStore(t), "api", fptr(2), iptr(gb))
	crossRef, err := SetVariable(st, nil, place("web"), "DB_COPY", "${{db.URL}}", now)
	if err != nil {
		t.Fatal(err)
	}
	del, err := DeleteVariable(st, place("api"), "PLAIN", now)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []changeset.Change{limits, crossRef, del, Restart(place("api"), now)} {
		if err := reg.CheckData(c); err != nil {
			t.Errorf("%s %s: CheckData = %v, want valid", c.Kind, c.Target, err)
		}
	}

	corrupt := limits
	corrupt.New = json.RawMessage(`{"cpu":2}`)
	badSet := crossRef
	badSet.New = json.RawMessage(`5`)
	noValue := crossRef
	noValue.New = json.RawMessage(`{"refs":[]}`)
	badName := del
	badName.Target = "1BAD"
	unknown := limits
	unknown.Kind = "service.teleport"
	for _, tc := range []struct {
		c    changeset.Change
		want string
	}{
		{corrupt, "limits.set: bad change data"},
		{badSet, "variable.set: bad change data"},
		{noValue, "variable.set: bad change data"},
		{badName, "invalid variable name"},
		{unknown, "unknown change kind service.teleport"},
	} {
		err := reg.CheckData(tc.c)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: CheckData = %v, want %q", tc.c.Kind, err, tc.want)
		}
	}
}
