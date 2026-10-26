package railway_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
)

func TestDomainMutationsWriteGuarded(t *testing.T) {
	f := railwaytest.New(t)
	c := newClient(f)
	c.WriteAllowed = func(string) bool { return false }
	ctx := context.Background()
	calls := map[string]func() error{
		"ServiceDomainCreate": func() error { _, err := c.ServiceDomainCreate(ctx, "e1", "s1", 8080); return err },
		"ServiceDomainDelete": func() error { return c.ServiceDomainDelete(ctx, "e1", "d1") },
		"CustomDomainCreate":  func() error { _, err := c.CustomDomainCreate(ctx, "p1", "e1", "s1", "a.example.test", 0); return err },
		"CustomDomainDelete":  func() error { return c.CustomDomainDelete(ctx, "e1", "d1") },
	}
	for name, call := range calls {
		var wn *railway.WriteNotAllowedError
		if err := call(); !errors.As(err, &wn) {
			t.Errorf("%s: err = %v, want *WriteNotAllowedError", name, err)
		}
	}
	if m := f.Mutations(); len(m) != 0 {
		t.Errorf("mutations recorded: %+v", m)
	}
}

func TestDomainMutationsNotRetried(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(502)
	}))
	defer srv.Close()
	c := &railway.Client{
		T:            &railway.Transport{Endpoint: srv.URL, Tokens: staticTokens{}},
		WriteAllowed: func(string) bool { return true },
	}
	ctx := context.Background()
	calls := map[string]func() error{
		"ServiceDomainCreate": func() error { _, err := c.ServiceDomainCreate(ctx, "e1", "s1", 0); return err },
		"ServiceDomainDelete": func() error { return c.ServiceDomainDelete(ctx, "e1", "d1") },
		"CustomDomainCreate":  func() error { _, err := c.CustomDomainCreate(ctx, "p1", "e1", "s1", "a.example.test", 80); return err },
		"CustomDomainDelete":  func() error { return c.CustomDomainDelete(ctx, "e1", "d1") },
	}
	for name, call := range calls {
		atomic.StoreInt32(&n, 0)
		if err := call(); err == nil {
			t.Errorf("%s: no error on 502", name)
		}
		if got := atomic.LoadInt32(&n); got != 1 {
			t.Errorf("%s sent %d times, want 1", name, got)
		}
	}
}

func TestCustomDomainRoundTrip(t *testing.T) {
	f := railwaytest.New(t)
	c := newClient(f)
	ctx := context.Background()

	cd, err := c.CustomDomainCreate(ctx, "p1", "e1", "s1", "a.example.test", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if cd.ID == "" || cd.Domain != "a.example.test" || cd.Port != 8080 {
		t.Fatalf("created = %+v", cd)
	}
	sd, err := c.ServiceDomainCreate(ctx, "e1", "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if sd.ID == "" || sd.Domain == "" {
		t.Fatalf("service domain = %+v", sd)
	}
	req, _ := f.LastRequest("ServiceDomainCreate")
	if in, _ := req.Vars["input"].(map[string]any); in == nil || in["targetPort"] != nil {
		t.Errorf("port 0 sent targetPort: %+v", req.Vars)
	}

	got, err := c.Domains(ctx, "p1", "e1", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Custom) != 1 || got.Custom[0].ID != cd.ID || len(got.Service) != 1 || got.Service[0].ID != sd.ID {
		t.Fatalf("Domains = %+v", got)
	}

	if err := c.CustomDomainDelete(ctx, "e1", cd.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.ServiceDomainDelete(ctx, "e1", sd.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = c.Domains(ctx, "p1", "e1", "s1")
	if len(got.Custom) != 0 || len(got.Service) != 0 {
		t.Fatalf("after delete: %+v", got)
	}

	f.FailNext("CustomDomainCreate", "boom")
	if _, err := c.CustomDomainCreate(ctx, "p1", "e1", "s1", "b.example.test", 0); err == nil {
		t.Error("FailNext not honoured")
	}
}

func TestDomainDeleteFalseIsError(t *testing.T) {
	f := railwaytest.New(t)
	c := newClient(f)
	ctx := context.Background()
	calls := map[string]func() error{
		"ServiceDomainDelete": func() error { return c.ServiceDomainDelete(ctx, "e1", "no-such") },
		"CustomDomainDelete":  func() error { return c.CustomDomainDelete(ctx, "e1", "no-such") },
	}
	for name, call := range calls {
		err := call()
		if err == nil || err.Error() != "Railway refused to delete the domain" {
			t.Errorf("%s of an unknown id: err = %v, want a refusal", name, err)
		}
	}
}
