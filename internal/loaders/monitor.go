package loaders

import (
	"context"
	"errors"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
)

// Polling cadences.
const (
	StatesEvery   = 10 * time.Second
	MetricsEvery  = 30 * time.Second
	MetricsWindow = 30 * time.Minute
	SampleSeconds = 30
	DetailDelay   = 300 * time.Millisecond
	LiveDebounce  = 250 * time.Millisecond
)

// Level is a panel chart resolution: one chart point is one sample of Step
// seconds. Name says it per point ("5m").
type Level struct {
	Name string
	Step int
}

// Levels are the panel chart resolutions, finest first.
var Levels = []Level{{"1m", 60}, {"5m", 300}, {"15m", 900}, {"1h", 3600}}

// MaxPanelCells bounds the chart cells a panel read covers, so a series
// never has more than 2 × MaxPanelCells points.
const MaxPanelCells = 200

// PanelSpan is how far back a panel read at step seconds goes for a chart
// cells wide: two points a Braille cell, cells capped at MaxPanelCells.
func PanelSpan(step, cells int) time.Duration {
	return time.Duration(2*max(0, min(cells, MaxPanelCells))*step) * time.Second
}

// partial splits err into the failure to record (nil when the read is usable)
// and the warning text of a *railway.PartialError (data came with it).
func partial(err error) (fail error, warn string) {
	var p *railway.PartialError
	if errors.As(err, &p) {
		return nil, strings.Join(p.Warnings, "; ")
	}
	return err, ""
}

// finish records a read: a failure keeps the last data, a partial answer
// counts as done with its warnings.
func finish(s *store.Store, key string, fail error, warn string, at time.Time) bool {
	if fail != nil {
		s.FailLoad(key, fail.Error())
		return false
	}
	if warn != "" {
		s.FinishLoadWarn(key, at, warn)
	} else {
		s.FinishLoad(key, at)
	}
	return true
}

// worst picks the error to report for two reads made together: a failure
// over a partial answer over nil.
func worst(e1, e2 error) error {
	switch {
	case e1 != nil && !isPartialErr(e1):
		return e1
	case e2 != nil && !isPartialErr(e2):
		return e2
	case e1 != nil:
		return e1
	}
	return e2
}

// StatesMsg is the deployment states of one project's environments.
type StatesMsg struct {
	ProjectID string
	// Key is the load status key; empty is store.StatesKey(ProjectID).
	Key    string
	EnvIDs []string
	States []railway.DeploymentState
	Err    error
	At     time.Time
}

// ApplyTo replaces the states; on failure the last ones are kept.
func (m StatesMsg) ApplyTo(s *store.Store) {
	key := m.Key
	if key == "" {
		key = store.StatesKey(m.ProjectID)
	}
	fail, warn := partial(m.Err)
	if finish(s, key, fail, warn, m.At) {
		s.ReplaceStates(m.ProjectID, m.EnvIDs, m.States)
	}
}

// MetricsMsg is the table metrics and volumes of one project.
type MetricsMsg struct {
	ProjectID string
	EnvIDs    []string
	Series    []railway.MetricSeries
	Volumes   []railway.VolumeMount
	Err       error
	At        time.Time
}

// ApplyTo replaces the metrics and volumes; on failure the last are kept.
func (m MetricsMsg) ApplyTo(s *store.Store) {
	fail, warn := partial(m.Err)
	if finish(s, store.MetricsKey(m.ProjectID), fail, warn, m.At) {
		s.ReplaceMetrics(m.EnvIDs, m.Series)
		s.ReplaceVolumes(m.EnvIDs, m.Volumes)
	}
}

// PanelMetricsMsg is the panel series of a place read at Step seconds over
// Span.
type PanelMetricsMsg struct {
	Place  domain.Place
	Step   int
	Span   time.Duration
	Series []railway.MetricSeries
	Err    error
	At     time.Time
}

// ApplyTo stores the panel series; on failure the last are kept.
func (m PanelMetricsMsg) ApplyTo(s *store.Store) {
	fail, warn := partial(m.Err)
	if finish(s, store.PanelKey(m.Place, m.Step, m.Span), fail, warn, m.At) {
		s.SetPanelSeries(m.Place, m.Step, m.Span, m.Series)
	}
}

// DetailMsg is a service instance's detail and domains.
type DetailMsg struct {
	Place   domain.Place
	Detail  railway.ServiceDetail
	Domains railway.Domains
	Err     error
	At      time.Time
}

// ApplyTo stores the detail and domains; on failure the last are kept.
func (m DetailMsg) ApplyTo(s *store.Store) {
	fail, warn := partial(m.Err)
	if finish(s, store.DetailKey(m.Place), fail, warn, m.At) {
		s.SetDetail(m.Place.EnvID, m.Place.ServiceID, m.Detail)
		s.SetDomains(m.Place, m.Domains)
	}
}

// ConfigMsg is one environment's config.
type ConfigMsg struct {
	EnvID  string
	Config railway.EnvConfig
	Err    error
	At     time.Time
}

// ApplyTo stores the config; on failure the last one is kept.
func (m ConfigMsg) ApplyTo(s *store.Store) {
	fail, warn := partial(m.Err)
	if finish(s, store.ConfigKey(m.EnvID), fail, warn, m.At) {
		s.SetConfig(m.EnvID, m.Config)
	}
}

var (
	_ store.Update = StatesMsg{}
	_ store.Update = MetricsMsg{}
	_ store.Update = PanelMetricsMsg{}
	_ store.Update = DetailMsg{}
	_ store.Update = ConfigMsg{}
)

// EnvIDs lists the environment ids of the targets.
func EnvIDs(targets []railway.DiscoveryTarget) []string {
	var out []string
	for _, t := range targets {
		for _, e := range t.Environments {
			out = append(out, e.ID)
		}
	}
	return out
}

func targetEnvIDs(t railway.DiscoveryTarget) []string {
	ids := make([]string, len(t.Environments))
	for i, e := range t.Environments {
		ids[i] = e.ID
	}
	return ids
}

// States reads the deployment states: one StatesMsg per project.
func States(c *railway.Client, targets []railway.DiscoveryTarget, now func() time.Time) tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(targets))
	for _, t := range targets {
		cmds = append(cmds, func() tea.Msg {
			st, err := c.ServiceStates(context.Background(), t.ProjectID)
			return StatesMsg{ProjectID: t.ProjectID, EnvIDs: targetEnvIDs(t), States: st, Err: err, At: now()}
		})
	}
	return tea.Batch(cmds...)
}

// PlaceStates reads the deployment states of p's project for p's
// environment only, recording the load status under store.PlaceStatesKey(p).
func PlaceStates(c *railway.Client, p domain.Place, now func() time.Time) tea.Cmd {
	return func() tea.Msg {
		st, err := c.ServiceStates(context.Background(), p.ProjectID)
		return StatesMsg{ProjectID: p.ProjectID, EnvIDs: []string{p.EnvID}, Key: store.PlaceStatesKey(p), States: st, Err: err, At: now()}
	}
}

// Metrics reads the table metrics (last MetricsWindow at SampleSeconds) and
// the volumes: one MetricsMsg per project.
func Metrics(c *railway.Client, targets []railway.DiscoveryTarget, now func() time.Time) tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(targets))
	for _, t := range targets {
		cmds = append(cmds, func() tea.Msg {
			ctx := context.Background()
			at := now()
			ser, e1 := c.ServiceMetrics(ctx, t.ProjectID, at.Add(-MetricsWindow), SampleSeconds)
			vols, e2 := c.ServiceVolumes(ctx, t.ProjectID)
			err := worst(e1, e2)
			return MetricsMsg{ProjectID: t.ProjectID, EnvIDs: targetEnvIDs(t), Series: ser, Volumes: vols, Err: err, At: at}
		})
	}
	return tea.Batch(cmds...)
}

func isPartialErr(err error) bool {
	var p *railway.PartialError
	return errors.As(err, &p)
}

// PanelMetrics reads a place's series at step seconds over the last span.
func PanelMetrics(c *railway.Client, p domain.Place, step int, span time.Duration, now func() time.Time) tea.Cmd {
	return func() tea.Msg {
		at := now()
		ser, err := c.ServiceMetricsOf(context.Background(), p.EnvID, p.ServiceID, at.Add(-span), step)
		return PanelMetricsMsg{Place: p, Step: step, Span: span, Series: ser, Err: err, At: at}
	}
}

// Detail reads a service instance's detail and domains together.
func Detail(c *railway.Client, p domain.Place, now func() time.Time) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		d, e1 := c.ServiceDetail(ctx, p.EnvID, p.ServiceID)
		dm, e2 := c.Domains(ctx, p.ProjectID, p.EnvID, p.ServiceID)
		return DetailMsg{Place: p, Detail: d, Domains: dm, Err: worst(e1, e2), At: now()}
	}
}

// Config reads the config of each environment: one ConfigMsg per env.
func Config(c *railway.Client, envIDs []string, now func() time.Time) tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(envIDs))
	for _, id := range envIDs {
		cmds = append(cmds, func() tea.Msg {
			cfg, err := c.EnvironmentConfig(context.Background(), id)
			return ConfigMsg{EnvID: id, Config: cfg, Err: err, At: now()}
		})
	}
	return tea.Batch(cmds...)
}
