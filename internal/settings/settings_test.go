package settings

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSettingsRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")

	d, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if d.Icons != "auto" || len(d.Projects) != 0 || len(d.Envs) != 0 || len(d.WriteEnvs) != 0 || len(d.Internal) != 0 {
		t.Errorf("defaults: %+v", d)
	}

	want := Settings{
		Projects: []string{"a"}, Envs: []string{"prod"}, WriteEnvs: []string{"dev"},
		Internal: []string{"x"}, Since: "2026-01-01", Icons: "nerd",
	}
	if err := Save(p, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v want %+v", got, want)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
}
