package kinds

import (
	"testing"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/railway"
)

// edited returns the fixture variables with fn applied to each; fn returns
// false to drop one. extra are appended.
func edited(fn func(r *railway.VarRecord) bool, extra ...railway.VarRecord) []railway.VarRecord {
	var out []railway.VarRecord
	for _, r := range fixtureVars() {
		if fn(&r) {
			out = append(out, r)
		}
	}
	return append(out, extra...)
}

func TestStoreDrift(t *testing.T) {
	keep := func(*railway.VarRecord) bool { return true }
	drop := func(svc, name string) func(*railway.VarRecord) bool {
		return func(r *railway.VarRecord) bool { return r.ServiceID != svc || r.Name != name }
	}
	set := func(svc, name, value string) func(t *testing.T) changeset.Change {
		return func(t *testing.T) changeset.Change {
			c, err := SetVariable(fixtureStore(t), nil, place(svc), name, value, now)
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	del := func(svc, name string) func(t *testing.T) changeset.Change {
		return func(t *testing.T) changeset.Change {
			c, err := DeleteVariable(fixtureStore(t), place(svc), name, now)
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	tests := []struct {
		name   string
		change func(t *testing.T) changeset.Change
		now    []railway.VarRecord
		want   string
	}{
		{"untouched", set("api", "PLAIN", "two"), fixtureVars(), ""},
		{"value changed", set("api", "PLAIN", "two"), edited(func(r *railway.VarRecord) bool {
			if r.Name == "PLAIN" {
				r.Unrendered = "other"
			}
			return true
		}), DriftValueChanged},
		{"already holds the new value", set("api", "PLAIN", "two"), edited(func(r *railway.VarRecord) bool {
			if r.Name == "PLAIN" {
				r.Unrendered = "two"
			}
			return true
		}), ""},
		{"turned sealed meanwhile", set("api", "PLAIN", "two"), edited(func(r *railway.VarRecord) bool {
			if r.Name == "PLAIN" {
				r.Unrendered, r.Sealed = "", true
			}
			return true
		}), DriftValueChanged},
		{"gone", set("api", "PLAIN", "two"), edited(drop("api", "PLAIN")), DriftGone},
		{"created meanwhile", set("api", "NEW", "n"), edited(keep,
			railway.VarRecord{EnvID: "e1", ServiceID: "api", Name: "NEW", Unrendered: "someone-else"}), DriftNowExists},
		{"created meanwhile with the new value", set("api", "NEW", "n"), edited(keep,
			railway.VarRecord{EnvID: "e1", ServiceID: "api", Name: "NEW", Unrendered: "n"}), ""},
		{"sealed base present", set("db", "KEY", "k2"), fixtureVars(), ""},
		{"sealed base gone", set("db", "KEY", "k2"), edited(drop("db", "KEY")), DriftGone},
		{"delete of a gone variable", del("api", "PLAIN"), edited(drop("api", "PLAIN")), ""},
		{"delete, value changed", del("api", "PLAIN"), edited(func(r *railway.VarRecord) bool {
			if r.Name == "PLAIN" {
				r.Unrendered = "other"
			}
			return true
		}), DriftValueChanged},
		{"delete with absent base, now exists", del("api", "NEW"), edited(keep,
			railway.VarRecord{EnvID: "e1", ServiceID: "api", Name: "NEW", Unrendered: "x"}), DriftNowExists},
		// The inventory skipped the place (env not read, partial read): its
		// absence means nothing.
		{"place not read", set("api", "PLAIN", "two"), edited(func(r *railway.VarRecord) bool { return r.ServiceID != "api" }), ""},
		{"env not read", set("api", "PLAIN", "two"), nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.change(t)
			s := fixtureStore(t)
			s.ReplaceVariables("p1", tt.now)
			if got := NewRegistry().StoreDrift(s, c); got != tt.want {
				t.Errorf("StoreDrift = %q, want %q", got, tt.want)
			}
		})
	}
}
