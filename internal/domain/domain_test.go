package domain

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseReferences(t *testing.T) {
	got := ParseReferences(`a ${{VAR}} ${{svc.VAR}} ${{"my svc".X.Y}} ${{shared.S}} ${{ api.server.TOKEN }}`)
	want := []Reference{
		{Raw: "${{VAR}}", Name: "VAR"},
		{Raw: "${{svc.VAR}}", Service: "svc", Name: "VAR"},
		{Raw: `${{"my svc".X.Y}}`, Service: "my svc", Name: "X.Y"},
		{Raw: "${{shared.S}}", Shared: true, Name: "S"},
		{Raw: "${{ api.server.TOKEN }}", Service: "api.server", Name: "TOKEN"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseReferences = %#v, want %#v", got, want)
	}
}

func TestIsLiteral(t *testing.T) {
	if !IsLiteral("plain value") {
		t.Errorf("IsLiteral(plain value) = false, want true")
	}
	if IsLiteral("x ${{A}}") {
		t.Errorf("IsLiteral(x ${{A}}) = true, want false")
	}
}

func TestFingerprint(t *testing.T) {
	long := strings.Repeat("x", 20)
	if Fingerprint("A", long, false, true, "e", "s") != Fingerprint("B", long, false, true, "e2", "s2") {
		t.Errorf("long literal values should group across names")
	}
	if n := len(Fingerprint("A", long, false, true, "e", "s")); n != 16 {
		t.Errorf("long literal fingerprint length = %d, want 16", n)
	}
	if Fingerprint("POSTGRES_USER", "postgres", false, true, "e", "s") == Fingerprint("POSTGRES_PASSWORD", "postgres", false, true, "e", "s") {
		t.Errorf("short values must not collide across names")
	}
	if !strings.HasPrefix(Fingerprint("P", "postgres", false, true, "e", "s"), "P:") {
		t.Errorf("short literal fingerprint should start with NAME:")
	}
	if got := Fingerprint("K", "", true, true, "e", "s"); got != "sealed:s:e:K" {
		t.Errorf("sealed fingerprint = %q, want sealed:s:e:K", got)
	}
	if got := Fingerprint("K", "", true, true, "e", ""); got != "sealed:shared:e:K" {
		t.Errorf("sealed shared fingerprint = %q, want sealed:shared:e:K", got)
	}
	if !strings.HasPrefix(Fingerprint("U", "${{a.B}}", false, false, "e", "s"), "ref:U:") {
		t.Errorf("reference fingerprint should start with ref:U:")
	}
}

func TestPlaceKey(t *testing.T) {
	if got := (Place{EnvID: "e", ServiceID: "s"}).Key(); got != "e/s" {
		t.Errorf("Key() = %q, want e/s", got)
	}
	if got := (Place{EnvID: "e"}).Key(); got != "e/shared" {
		t.Errorf("shared Key() = %q, want e/shared", got)
	}
	if !(Place{EnvID: "e"}).Shared() {
		t.Errorf("Shared() = false, want true for empty ServiceID")
	}
	if got := LocationID("e", "", "X"); got != "e/shared/X" {
		t.Errorf("LocationID = %q, want e/shared/X", got)
	}
}
