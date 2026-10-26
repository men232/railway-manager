package store

import (
	"reflect"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/railway"
)

func fixtureCatalog() railway.Catalog {
	return railway.Catalog{Projects: []railway.Project{
		{
			ID: "p1", Name: "Proj", Workspace: "w",
			Environments: []railway.Environment{{ID: "e1", Name: "prod", CanAccess: true, ServiceIDs: []string{"sapi", "sdb", "sweb", "sx", "sy"}}},
			Services: []railway.Service{
				{ID: "sapi", Name: "api"}, {ID: "sdb", Name: "db"}, {ID: "sweb", Name: "web"},
				{ID: "sx", Name: "x"}, {ID: "sy", Name: "y"},
			},
		},
		{
			ID: "p2", Name: "Other", Workspace: "w",
			Environments: []railway.Environment{{ID: "e2", Name: "dev", CanAccess: true, ServiceIDs: []string{"sz"}}},
			Services:     []railway.Service{{ID: "sz", Name: "z"}},
		},
	}}
}

func TestRevisionsBump(t *testing.T) {
	s := New()
	if s.Rev(Catalog) != 0 || s.Rev(Variables) != 0 || s.Rev(Loads) != 0 {
		t.Fatal("revisions should start at zero")
	}
	s.ReplaceCatalog(fixtureCatalog())
	if s.Rev(Catalog) != 1 || s.Rev(Variables) != 0 || s.Rev(Loads) != 0 {
		t.Errorf("after ReplaceCatalog: %d %d %d", s.Rev(Catalog), s.Rev(Variables), s.Rev(Loads))
	}
	s.ReplaceVariables("p1", nil)
	if s.Rev(Catalog) != 1 || s.Rev(Variables) != 1 || s.Rev(Loads) != 0 {
		t.Errorf("after ReplaceVariables: %d %d %d", s.Rev(Catalog), s.Rev(Variables), s.Rev(Loads))
	}
	s.BeginLoad("catalog")
	if s.Rev(Catalog) != 1 || s.Rev(Variables) != 1 || s.Rev(Loads) != 1 {
		t.Errorf("after BeginLoad: %d %d %d", s.Rev(Catalog), s.Rev(Variables), s.Rev(Loads))
	}
	s.ReplaceVariables("p1", []railway.VarRecord{{EnvID: "e1", ServiceID: "sapi", Name: "A", Unrendered: "1"}})
	s.ReplaceCatalog(fixtureCatalog())
	if s.Rev(Catalog) != 2 || s.Rev(Variables) != 3 || s.Rev(Loads) != 1 {
		t.Errorf("catalog replace with variables: %d %d %d", s.Rev(Catalog), s.Rev(Variables), s.Rev(Loads))
	}
}

func TestCatalogChangeUpdatesEdges(t *testing.T) {
	recs := []railway.VarRecord{
		{EnvID: "e1", ServiceID: "sdb", Name: "URL", Unrendered: "postgres://x"},
		{EnvID: "e1", ServiceID: "sapi", Name: "URL", Unrendered: "${{db.URL}}"},
	}
	dbURL := domain.LocationID("e1", "sdb", "URL")
	apiURL := domain.LocationID("e1", "sapi", "URL")

	t.Run("service renamed", func(t *testing.T) {
		s := New()
		s.ReplaceCatalog(fixtureCatalog())
		s.ReplaceVariables("p1", recs)
		if got := s.Dependents(dbURL); !reflect.DeepEqual(got, []string{apiURL}) {
			t.Fatalf("before rename: %v", got)
		}
		rev := s.Rev(Variables)
		c := fixtureCatalog()
		c.Projects[0].Services[1].Name = "db2"
		s.ReplaceCatalog(c)
		if got := s.Dependents(dbURL); len(got) != 0 {
			t.Errorf("edge should vanish after rename: %v", got)
		}
		if s.Rev(Variables) <= rev {
			t.Error("Variables revision should grow")
		}
	})

	t.Run("variables before catalog", func(t *testing.T) {
		s := New()
		s.ReplaceVariables("p1", recs)
		if got := s.Dependents(dbURL); len(got) != 0 {
			t.Errorf("edges should be unresolved: %v", got)
		}
		s.ReplaceCatalog(fixtureCatalog())
		if got := s.Dependents(dbURL); !reflect.DeepEqual(got, []string{apiURL}) {
			t.Errorf("edge should appear: %v", got)
		}
	})
}

func TestVariablesIndexes(t *testing.T) {
	s := New()
	s.ReplaceCatalog(fixtureCatalog())
	s.ReplaceVariables("p1", []railway.VarRecord{
		{EnvID: "e1", ServiceID: "sapi", Name: "B", Unrendered: "b"},
		{EnvID: "e1", ServiceID: "sapi", Name: "A", Unrendered: "a"},
		{EnvID: "e1", ServiceID: "", Name: "S", Unrendered: "shared-value-that-is-long"},
		{EnvID: "e1", ServiceID: "sapi", Name: "SECRET", Unrendered: "hidden", Sealed: true},
	})
	got := s.VariablesAt(domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "sapi"})
	var names []string
	for _, v := range got {
		names = append(names, v.Name)
	}
	if want := []string{"A", "B", "SECRET"}; !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	shared := s.VariablesAt(domain.Place{EnvID: "e1"})
	if len(shared) != 1 || shared[0].Name != "S" || !shared[0].Place.Shared() {
		t.Fatalf("shared = %+v", shared)
	}
	sealed := s.Variable(domain.LocationID("e1", "sapi", "SECRET"))
	if sealed == nil || !sealed.Sealed || sealed.Fingerprint != "sealed:sapi:e1:SECRET" {
		t.Errorf("sealed = %+v", sealed)
	}
	if sealed.Place.ProjectID != "p1" || !sealed.Literal {
		t.Errorf("sealed place/literal = %+v", sealed)
	}
	if s.Variable("nope") != nil {
		t.Error("unknown location should be nil")
	}
}

func TestReferencesAndDependents(t *testing.T) {
	s := New()
	s.ReplaceCatalog(fixtureCatalog())
	s.ReplaceVariables("p1", []railway.VarRecord{
		{EnvID: "e1", ServiceID: "sdb", Name: "URL", Unrendered: "postgres://x"},
		{EnvID: "e1", ServiceID: "sapi", Name: "URL", Unrendered: "${{db.URL}}"},
		{EnvID: "e1", ServiceID: "sweb", Name: "API", Unrendered: "${{api.URL}}"},
		{EnvID: "e1", ServiceID: "sx", Name: "A", Unrendered: "${{y.B}}"},
		{EnvID: "e1", ServiceID: "sy", Name: "B", Unrendered: "${{x.A}}"},
		{EnvID: "e1", ServiceID: "", Name: "S", Unrendered: "s"},
	})
	dbURL := domain.LocationID("e1", "sdb", "URL")
	want := []string{domain.LocationID("e1", "sapi", "URL"), domain.LocationID("e1", "sweb", "API")}
	if got := s.Dependents(dbURL); !reflect.DeepEqual(got, want) {
		t.Errorf("Dependents = %v, want %v", got, want)
	}
	if v := s.Variable(dbURL); !v.Literal {
		t.Error("db.URL should be literal")
	}
	if v := s.Variable(domain.LocationID("e1", "sapi", "URL")); v.Literal {
		t.Error("api.URL should not be literal")
	}

	xA := domain.LocationID("e1", "sx", "A")
	yB := domain.LocationID("e1", "sy", "B")
	if got, want := s.Dependents(xA), []string{xA, yB}; !reflect.DeepEqual(got, want) {
		t.Errorf("cycle Dependents = %v, want %v", got, want)
	}

	from := domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "sapi"}
	if got, want := s.ResolveReference(from, domain.Reference{Shared: true, Name: "S"}), domain.LocationID("e1", "", "S"); got != want {
		t.Errorf("shared resolve = %q, want %q", got, want)
	}
	if got := s.ResolveReference(from, domain.Reference{Service: "ghost", Name: "S"}); got != "" {
		t.Errorf("unknown service = %q", got)
	}
	if got, want := s.ResolveReference(from, domain.Reference{Service: "db", Name: "URL"}), dbURL; got != want {
		t.Errorf("service resolve = %q, want %q", got, want)
	}
	if got, want := s.ResolveReference(from, domain.Reference{Name: "URL"}), domain.LocationID("e1", "sapi", "URL"); got != want {
		t.Errorf("same-service resolve = %q, want %q", got, want)
	}
}

func TestReplaceVariablesPerProject(t *testing.T) {
	s := New()
	s.ReplaceCatalog(fixtureCatalog())
	s.ReplaceVariables("p1", []railway.VarRecord{{EnvID: "e1", ServiceID: "sapi", Name: "A", Unrendered: "1"}})
	s.ReplaceVariables("p2", []railway.VarRecord{{EnvID: "e2", ServiceID: "sz", Name: "Z", Unrendered: "2"}})
	s.ReplaceVariables("p1", []railway.VarRecord{{EnvID: "e1", ServiceID: "sapi", Name: "B", Unrendered: "3"}})
	if s.Variable(domain.LocationID("e1", "sapi", "A")) != nil {
		t.Error("old p1 variable should be gone")
	}
	if s.Variable(domain.LocationID("e1", "sapi", "B")) == nil {
		t.Error("new p1 variable missing")
	}
	if v := s.Variable(domain.LocationID("e2", "sz", "Z")); v == nil || v.Place.ProjectID != "p2" {
		t.Errorf("p2 variable lost: %+v", v)
	}
	if got := s.VariablesAt(domain.Place{EnvID: "e1", ServiceID: "sapi"}); len(got) != 1 {
		t.Errorf("byPlace stale: %d entries", len(got))
	}
}

func TestLookups(t *testing.T) {
	s := New()
	s.ReplaceCatalog(fixtureCatalog())
	if got := s.EnvLabel("e1"); got != "Proj/prod" {
		t.Errorf("EnvLabel = %q", got)
	}
	if got := s.EnvName("e1"); got != "prod" {
		t.Errorf("EnvName = %q", got)
	}
	if got := s.EnvName("nope"); got != "nope" {
		t.Errorf("EnvName of an unknown env = %q", got)
	}
	if s.ServiceName("") != "shared" || s.ServiceName("sdb") != "db" {
		t.Error("ServiceName")
	}
	if s.ServiceIDByName("e1", "web") != "sweb" || s.ServiceIDByName("e1", "zzz") != "" || s.ServiceIDByName("e2", "web") != "" {
		t.Error("ServiceIDByName")
	}
	if got, want := s.PlaceOf("e2", "sz"), (domain.Place{ProjectID: "p2", EnvID: "e2", ServiceID: "sz"}); got != want {
		t.Errorf("PlaceOf = %+v", got)
	}
	inst := s.Instances([]string{"e2", "e1"})
	if len(inst) != 6 || inst[0].Service != "api" || inst[0].Project != "Proj" || inst[5].Service != "z" || inst[5].Env != "dev" {
		t.Errorf("Instances = %+v", inst)
	}
	if len(s.Projects()) != 2 {
		t.Error("Projects")
	}
}

func TestLoadState(t *testing.T) {
	s := New()
	if s.Load("catalog") != (LoadState{}) {
		t.Error("zero state expected")
	}
	s.BeginLoad("catalog")
	if !s.Load("catalog").Loading {
		t.Error("should be loading")
	}
	s.FailLoad("catalog", "boom")
	if st := s.Load("catalog"); st.Loading || st.Err != "boom" {
		t.Errorf("after fail: %+v", st)
	}
	at := time.Unix(100, 0)
	s.BeginLoad("catalog")
	s.FinishLoad("catalog", at)
	if st := s.Load("catalog"); st.Loading || st.Err != "" || !st.ReadAt.Equal(at) {
		t.Errorf("after finish: %+v", st)
	}
	s.BeginLoad("catalog")
	s.FailLoad("catalog", "again")
	if !s.Load("catalog").ReadAt.Equal(at) {
		t.Error("failed read must keep last ReadAt")
	}
}

func TestCloneIsDeep(t *testing.T) {
	s := New()
	s.ReplaceCatalog(fixtureCatalog())
	s.ReplaceVariables("p1", []railway.VarRecord{
		{EnvID: "e1", ServiceID: "sapi", Name: "A", Unrendered: "${{db.B}}"},
		{EnvID: "e1", ServiceID: "sdb", Name: "B", Unrendered: "b"},
	})
	s.FinishLoad("catalog", time.Unix(1, 0))
	c := s.Clone()
	if !reflect.DeepEqual(s, c) {
		t.Fatalf("clone differs from original")
	}
	if c.Projects()[0].Name != "Proj" || c.EnvLabel("e1") != "Proj/prod" || len(c.Dependents("e1/sdb/B")) != 1 {
		t.Fatalf("clone lookups broken")
	}

	// mutating the clone leaves the original unchanged
	c.Variable("e1/sapi/A").Unrendered = "changed"
	c.projects[0].Environments[0].ServiceIDs[0] = "changed"
	c.projects[0].Name = "changed"
	c.ReplaceVariables("p1", nil)
	c.ReplaceCatalog(railway.Catalog{})
	c.FailLoad("catalog", "x")
	if v := s.Variable("e1/sapi/A"); v == nil || v.Unrendered != "${{db.B}}" {
		t.Errorf("original variable changed: %+v", v)
	}
	if s.Projects()[0].Environments[0].ServiceIDs[0] != "sapi" || s.EnvLabel("e1") != "Proj/prod" {
		t.Errorf("original catalog changed")
	}
	if len(s.Dependents("e1/sdb/B")) != 1 || s.Load("catalog").Err != "" || s.Rev(Variables) == c.Rev(Variables) {
		t.Errorf("original indexes or loads changed")
	}
}

func TestSystemVars(t *testing.T) {
	s := New()
	s.ReplaceCatalog(fixtureCatalog())
	s.ReplaceVariables("p1",
		[]railway.VarRecord{{EnvID: "e1", ServiceID: "sdb", Name: "A", Unrendered: "1"}},
		railway.SystemRecord{EnvID: "e1", ServiceID: "sdb", Name: "RAILWAY_PRIVATE_DOMAIN", Value: "db.railway.internal"},
	)
	s.ReplaceVariables("p2", nil, railway.SystemRecord{EnvID: "e2", ServiceID: "sz", Name: "RAILWAY_PRIVATE_DOMAIN", Value: "z.railway.internal"})
	if got := s.SystemVar("e1", "sdb", "RAILWAY_PRIVATE_DOMAIN"); got != "db.railway.internal" {
		t.Errorf("SystemVar = %q", got)
	}
	if got := s.SystemVar("e1", "sdb", "RAILWAY_TCP_PROXY_PORT"); got != "" {
		t.Errorf("unknown SystemVar = %q", got)
	}
	// system values are not variables
	if s.Variable(domain.LocationID("e1", "sdb", "RAILWAY_PRIVATE_DOMAIN")) != nil {
		t.Error("system value became a variable")
	}
	if got := s.VariablesAt(domain.Place{EnvID: "e1", ServiceID: "sdb"}); len(got) != 1 {
		t.Errorf("VariablesAt = %d, want 1", len(got))
	}

	c := s.Clone()
	if !reflect.DeepEqual(s, c) {
		t.Fatal("clone differs from original")
	}
	rev := s.Rev(Variables)
	// replaced with the project's variables; other projects kept
	s.ReplaceVariables("p1", nil)
	if s.Rev(Variables) != rev+1 {
		t.Errorf("Variables rev = %d, want %d", s.Rev(Variables), rev+1)
	}
	if got := s.SystemVar("e1", "sdb", "RAILWAY_PRIVATE_DOMAIN"); got != "" {
		t.Errorf("stale SystemVar = %q", got)
	}
	if got := s.SystemVar("e2", "sz", "RAILWAY_PRIVATE_DOMAIN"); got != "z.railway.internal" {
		t.Errorf("p2 SystemVar lost: %q", got)
	}
	if got := c.SystemVar("e1", "sdb", "RAILWAY_PRIVATE_DOMAIN"); got != "db.railway.internal" {
		t.Errorf("clone shares system values: %q", got)
	}
}
