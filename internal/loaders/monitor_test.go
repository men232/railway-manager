package loaders_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
)

var t0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func TestStatesMsgApplies(t *testing.T) {
	st := store.New()
	loaders.StatesMsg{ProjectID: "p", EnvIDs: []string{"e"}, At: t0,
		States: []railway.DeploymentState{{EnvID: "e", ServiceID: "s", ActiveStatus: "SUCCESS"}}}.ApplyTo(st)
	if d, ok := st.State("e", "s"); !ok || d.ActiveStatus != "SUCCESS" {
		t.Errorf("state = %+v, %v", d, ok)
	}
	if !st.StatesRead("e") || !st.Load(store.StatesKey("p")).ReadAt.Equal(t0) {
		t.Errorf("load = %+v", st.Load(store.StatesKey("p")))
	}
}

func TestMetricsMsgAppliesSeriesAndVolumes(t *testing.T) {
	st := store.New()
	loaders.MetricsMsg{ProjectID: "p", EnvIDs: []string{"e"}, At: t0,
		Series:  []railway.MetricSeries{{EnvID: "e", ServiceID: "s", Measurement: "CPU_USAGE", Points: []railway.MetricPoint{{TS: 1, Value: 2}}}},
		Volumes: []railway.VolumeMount{{EnvID: "e", ServiceID: "s"}}}.ApplyTo(st)
	if got := st.Series("e", "s", "CPU_USAGE"); len(got) != 1 || got[0].Value != 2 {
		t.Errorf("series = %+v", got)
	}
	if len(st.Volumes("e", "s")) != 1 {
		t.Errorf("volumes = %+v", st.Volumes("e", "s"))
	}
}

func TestPartialErrorAppliesWithWarning(t *testing.T) {
	st := store.New()
	loaders.StatesMsg{ProjectID: "p", EnvIDs: []string{"e"}, At: t0,
		States: []railway.DeploymentState{{EnvID: "e", ServiceID: "s"}},
		Err:    &railway.PartialError{Warnings: []string{"env x denied", "svc y denied"}}}.ApplyTo(st)
	if _, ok := st.State("e", "s"); !ok {
		t.Fatal("partial data not applied")
	}
	l := st.Load(store.StatesKey("p"))
	if l.Err != "! env x denied; svc y denied" || !l.ReadAt.Equal(t0) || l.Loading {
		t.Errorf("load = %+v", l)
	}
}

func TestFailedReadKeepsLastData(t *testing.T) {
	st := store.New()
	p := domain.Place{ProjectID: "p", EnvID: "e", ServiceID: "s"}
	loaders.StatesMsg{ProjectID: "p", EnvIDs: []string{"e"}, At: t0, States: []railway.DeploymentState{{EnvID: "e", ServiceID: "s", ActiveStatus: "SUCCESS"}}}.ApplyTo(st)
	loaders.MetricsMsg{ProjectID: "p", EnvIDs: []string{"e"}, At: t0, Series: []railway.MetricSeries{{EnvID: "e", ServiceID: "s", Measurement: "M", Points: []railway.MetricPoint{{TS: 1}}}}}.ApplyTo(st)
	loaders.DetailMsg{Place: p, At: t0, Detail: railway.ServiceDetail{}}.ApplyTo(st)
	loaders.ConfigMsg{EnvID: "e", At: t0, Config: railway.EnvConfig{Etag: "a"}}.ApplyTo(st)
	loaders.PanelMetricsMsg{Place: p, Step: 60, Span: time.Hour, At: t0, Series: []railway.MetricSeries{{Measurement: "M", Points: []railway.MetricPoint{{TS: 1}}}}}.ApplyTo(st)

	boom := errors.New("boom")
	later := t0.Add(time.Minute)
	loaders.StatesMsg{ProjectID: "p", EnvIDs: []string{"e"}, Err: boom, At: later}.ApplyTo(st)
	loaders.MetricsMsg{ProjectID: "p", EnvIDs: []string{"e"}, Err: boom, At: later}.ApplyTo(st)
	loaders.DetailMsg{Place: p, Err: boom, At: later}.ApplyTo(st)
	loaders.ConfigMsg{EnvID: "e", Err: boom, At: later}.ApplyTo(st)
	loaders.PanelMetricsMsg{Place: p, Step: 60, Span: time.Hour, Err: boom, At: later}.ApplyTo(st)

	if d, ok := st.State("e", "s"); !ok || d.ActiveStatus != "SUCCESS" {
		t.Errorf("state lost: %+v", d)
	}
	if len(st.Series("e", "s", "M")) != 1 || len(st.PanelSeries(p, 60, time.Hour, "M")) != 1 {
		t.Error("series lost")
	}
	if _, ok := st.Detail("e", "s"); !ok {
		t.Error("detail lost")
	}
	if c, ok := st.Config("e"); !ok || c.Etag != "a" {
		t.Errorf("config lost: %+v", c)
	}
	for _, k := range []string{store.StatesKey("p"), store.MetricsKey("p"), store.DetailKey(p), store.ConfigKey("e"), store.PanelKey(p, 60, time.Hour)} {
		l := st.Load(k)
		if l.Err != "boom" || !l.ReadAt.Equal(t0) {
			t.Errorf("%s load = %+v", k, l)
		}
	}
}

func TestDetailMsgAppliesDomains(t *testing.T) {
	st := store.New()
	p := domain.Place{ProjectID: "p", EnvID: "e", ServiceID: "s"}
	loaders.DetailMsg{Place: p, At: t0,
		Detail:  railway.ServiceDetail{Regions: map[string]int{"us": 1}},
		Domains: railway.Domains{Service: []railway.ServiceDomain{{Domain: "a.up.railway.app"}}}}.ApplyTo(st)
	if d, ok := st.Domains(p); !ok || len(d.Service) != 1 {
		t.Errorf("domains = %+v, %v", d, ok)
	}
	if d, ok := st.Detail("e", "s"); !ok || d.Regions["us"] != 1 {
		t.Errorf("detail = %+v", d)
	}
}

func TestKeys(t *testing.T) {
	p := domain.Place{EnvID: "e", ServiceID: "s"}
	if store.StatesKey("p") != "states:p" || store.MetricsKey("p") != "metrics:p" ||
		store.DetailKey(p) != "detail:e/s" || store.ConfigKey("e") != "config:e" ||
		store.PanelKey(p, 300, 2*time.Hour) != "panel:e/s/300s/7200s" {
		t.Error("keys changed")
	}
}

// capture serves an empty metrics answer and records the request variables.
func capture(t *testing.T) (*railway.Client, *map[string]any) {
	t.Helper()
	got := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct{ Variables map[string]any }
		_ = json.Unmarshal(b, &req)
		for k, v := range req.Variables {
			got[k] = v
		}
		_, _ = w.Write([]byte(`{"data":{"metrics":[]}}`))
	}))
	t.Cleanup(srv.Close)
	return &railway.Client{T: &railway.Transport{Endpoint: srv.URL, Tokens: staticTokens{}}}, &got
}

func TestMetricsWindowIsThirtyMinutes(t *testing.T) {
	c, got := capture(t)
	tg := []railway.DiscoveryTarget{{ProjectID: "p", Environments: []railway.Environment{{ID: "e"}}}}
	msgs := runBatch(loaders.Metrics(c, tg, func() time.Time { return t0 }))
	if len(msgs) != 1 {
		t.Fatalf("msgs = %d", len(msgs))
	}
	if (*got)["startDate"] != t0.Add(-30*time.Minute).Format(time.RFC3339) || (*got)["sampleRateSeconds"] != float64(30) {
		t.Errorf("vars = %v", *got)
	}
	if loaders.MetricsWindow != 30*time.Minute || loaders.SampleSeconds != 30 {
		t.Error("constants")
	}
}

func TestPanelMetricsUsesStepAndSpan(t *testing.T) {
	c, got := capture(t)
	p := domain.Place{ProjectID: "p", EnvID: "e", ServiceID: "s"}
	span := loaders.PanelSpan(300, 34)
	m := loaders.PanelMetrics(c, p, 300, span, func() time.Time { return t0 })().(loaders.PanelMetricsMsg)
	if m.Err != nil || m.Step != 300 || m.Span != span || m.Place != p {
		t.Fatalf("msg = %+v", m)
	}
	if (*got)["startDate"] != t0.Add(-span).Format(time.RFC3339) || (*got)["sampleRateSeconds"] != float64(300) {
		t.Errorf("vars = %v", *got)
	}
}

// The panel levels are point resolutions; the span is two points a chart
// cell, the cells capped so a series stays a few hundred points.
func TestPanelLevelsAndSpan(t *testing.T) {
	want := []loaders.Level{{"1m", 60}, {"5m", 300}, {"15m", 900}, {"1h", 3600}}
	if !slices.Equal(loaders.Levels, want) {
		t.Errorf("Levels = %v", loaders.Levels)
	}
	for _, l := range loaders.Levels {
		for _, cells := range []int{1, 17, 47, 100, 197, loaders.MaxPanelCells} {
			if got, want := loaders.PanelSpan(l.Step, cells), time.Duration(2*cells*l.Step)*time.Second; got != want {
				t.Errorf("PanelSpan(%d, %d) = %v, want %v", l.Step, cells, got, want)
			}
		}
		if got := loaders.PanelSpan(l.Step, 500); got != loaders.PanelSpan(l.Step, loaders.MaxPanelCells) {
			t.Errorf("PanelSpan(%d, 500) = %v, not capped", l.Step, got)
		}
		if got := loaders.PanelSpan(l.Step, 0); got != 0 {
			t.Errorf("PanelSpan(%d, 0) = %v", l.Step, got)
		}
	}
	if loaders.MaxPanelCells != 200 {
		t.Errorf("MaxPanelCells = %d", loaders.MaxPanelCells)
	}
}

func runBatch(cmd func() tea.Msg) []tea.Msg {
	msg := cmd()
	if b, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range b {
			out = append(out, c())
		}
		return out
	}
	return []tea.Msg{msg}
}

// A states read made for one place (the Logs screen) records its load status
// under its own key, never the project-wide one the Services screen shows.
func TestPlaceStatesOwnLoadKey(t *testing.T) {
	st := store.New()
	p := domain.Place{ProjectID: "p", EnvID: "e", ServiceID: "s"}
	loaders.StatesMsg{ProjectID: "p", EnvIDs: []string{"e"}, Key: store.PlaceStatesKey(p), At: t0,
		Err: errors.New("down")}.ApplyTo(st)
	if l := st.Load(store.StatesKey("p")); l.Err != "" {
		t.Errorf("project load = %+v", l)
	}
	if l := st.Load(store.PlaceStatesKey(p)); l.Err != "down" {
		t.Errorf("place load = %+v", l)
	}
	loaders.StatesMsg{ProjectID: "p", EnvIDs: []string{"e"}, Key: store.PlaceStatesKey(p), At: t0,
		States: []railway.DeploymentState{{EnvID: "e", ServiceID: "s", ActiveStatus: "SUCCESS"}}}.ApplyTo(st)
	if d, ok := st.State("e", "s"); !ok || d.ActiveStatus != "SUCCESS" {
		t.Errorf("state = %+v, %v", d, ok)
	}
	if l := st.Load(store.StatesKey("p")); !l.ReadAt.IsZero() {
		t.Errorf("project load touched: %+v", l)
	}
}
