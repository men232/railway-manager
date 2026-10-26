package view

import (
	"cmp"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/monitor"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
)

// Change kinds and target prefixes the monitor selectors match on. They are
// the kinds package's names; this package does not import it.
const (
	kindLimits        = "limits.set"
	kindVolume        = "volume.size"
	kindServiceDomDel = "domain.service.delete"
	kindServiceDomAdd = "domain.service.add"
	kindCustomDomAdd  = "domain.custom.add"
	kindCustomDomDel  = "domain.custom.delete"

	volumeTarget       = "volume:"
	customDomainTarget = "custom-domain:"
	serviceDomainTgt   = "service-domain:"
)

// MonitorRow is one service of one environment with its status, newest
// metric series and pending changes. The series are oldest first; Net is in
// bytes/s. Limits are 0 when unknown.
type MonitorRow struct {
	Place                 domain.Place
	Project, Env, Service string
	Status                monitor.Status
	CPU, Mem, Net         []float64
	CPULimit, MemLimitGB  float64
	Pending               int
	// LimitsOverlay is the short overlay of a pending limits change, or "".
	LimitsOverlay string
}

// ChartView is one panel chart: cpu, mem or net. Limit is 0 when unknown;
// for net it is the series' peak (the colour scale of traffic).
type ChartView struct {
	Name              string
	Values            []float64
	Last, Peak, Limit float64
	HasData           bool
}

// LimitsView is a service's CPU/memory override.
type LimitsView struct {
	CPU, MemBytes float64
	Override      bool
	Pending       *apply.Overlay
}

// VolumeView is one mounted volume. Sizes are MB.
type VolumeView struct {
	ID, MountPath  string
	SizeMB, UsedMB float64
	Pending        *apply.Overlay
}

// DomainView is one domain row. A pending add has no ID yet (and a Railway
// domain add no host); a pending delete keeps its row. ChangeID is the
// pending change's id ("" for none); a pending add's Port is the one it asks
// for. CertStatus is Railway's certificate status of a custom domain, as
// read.
type DomainView struct {
	Host             string
	Port             int
	Custom, Verified bool
	Pending          *apply.Overlay
	ID               string
	ChangeID         string
	CertStatus       string
}

// Detail is the detail panel of one service instance.
type Detail struct {
	Place   domain.Place
	Info    railway.ServiceDetail
	HasInfo bool
	Limits  LimitsView
	Volumes []VolumeView
	Domains []DomainView
	Step    int           // the charts' sample step in seconds
	Span    time.Duration // how far back the charts go
	Charts  []ChartView
}

func sortedPoints(pts []railway.MetricPoint) []railway.MetricPoint {
	if slices.IsSortedFunc(pts, func(a, b railway.MetricPoint) int { return cmp.Compare(a.TS, b.TS) }) {
		return pts
	}
	return slices.SortedStableFunc(slices.Values(pts), func(a, b railway.MetricPoint) int { return cmp.Compare(a.TS, b.TS) })
}

func values(pts []railway.MetricPoint) []float64 {
	pts = sortedPoints(pts)
	out := make([]float64, len(pts))
	for i, p := range pts {
		out[i] = p.Value
	}
	return out
}

func newestValue(pts []railway.MetricPoint) float64 {
	if len(pts) == 0 {
		return 0
	}
	return slices.MaxFunc(pts, func(a, b railway.MetricPoint) int { return cmp.Compare(a.TS, b.TS) }).Value
}

func (v *Views) overlay(c changeset.Change) *apply.Overlay {
	kd, ok := v.Reg[c.Kind]
	if !ok {
		return nil
	}
	o := kd.Overlay(c)
	return &o
}

// Monitor lists the services of the environments with status, metrics and
// pending changes. The rows are built once per store revision (catalog,
// deploys, metrics, configs), change set revision and environment set: the
// Services screen reads them several times a frame.
func (v *Views) Monitor(cs *changeset.ChangeSet, envIDs []string) []MonitorRow {
	k := key(append(rev(v.Store, store.Catalog, store.Deploys, store.Metrics, store.Configs),
		csRev(cs), strings.Join(envIDs, ","))...)
	return v.monitor.Get(k, func() []MonitorRow {
		v.monitorBuilds++
		pending := map[string]int{}
		limits := map[string]string{}
		for _, c := range changes(cs) {
			pending[c.Place.Key()]++
			if c.Kind == kindLimits {
				if o := v.overlay(c); o != nil {
					limits[c.Place.Key()] = o.Short
				}
			}
		}
		insts := v.Store.Instances(envIDs)
		rows := make([]MonitorRow, len(insts))
		for i, in := range insts {
			env, svc := in.Place.EnvID, in.Place.ServiceID
			st, _ := v.Store.State(env, svc)
			rx, tx := v.Store.Series(env, svc, "NETWORK_RX_GB"), v.Store.Series(env, svc, "NETWORK_TX_GB")
			pk := in.Place.Key()
			rows[i] = MonitorRow{
				Place: in.Place, Project: in.Project, Env: in.Env, Service: in.Service,
				Status:     monitor.StatusOf(st, v.Store.StatesRead(env)),
				CPU:        values(v.Store.Series(env, svc, "CPU_USAGE")),
				Mem:        values(v.Store.Series(env, svc, "MEMORY_USAGE_GB")),
				Net:        monitor.Rate(rx, tx),
				CPULimit:   newestValue(v.Store.Series(env, svc, "CPU_LIMIT")),
				MemLimitGB: newestValue(v.Store.Series(env, svc, "MEMORY_LIMIT_GB")),
				Pending:    pending[pk], LimitsOverlay: limits[pk],
			}
		}
		return rows
	})
}

// Detail builds the detail panel of a service instance with the charts read
// at step seconds over span.
func (v *Views) Detail(cs *changeset.ChangeSet, p domain.Place, step int, span time.Duration) Detail {
	k := key(append(rev(v.Store, store.Catalog, store.Details, store.DomainsCol, store.Volumes, store.Configs,
		store.Metrics, store.PanelMetrics, store.Deploys), csRev(cs), p.Key(), p.ProjectID, strconv.Itoa(step), span.String())...)
	return v.detail.Get(k, func() Detail {
		d := Detail{Place: p, Step: step, Span: span}
		d.Info, d.HasInfo = v.Store.Detail(p.EnvID, p.ServiceID)

		var pend []changeset.Change
		for _, c := range changes(cs) {
			if c.Place.Key() == p.Key() {
				pend = append(pend, c)
			}
		}
		find := func(kind, target string) *apply.Overlay {
			for _, c := range pend {
				if c.Kind == kind && strings.EqualFold(c.Target, target) {
					return v.overlay(c)
				}
			}
			return nil
		}

		if cfg, ok := v.Store.Config(p.EnvID); ok {
			sc := cfg.Services[p.ServiceID]
			if sc.CPU != nil {
				d.Limits.CPU, d.Limits.Override = *sc.CPU, true
			}
			if sc.MemoryBytes != nil {
				d.Limits.MemBytes, d.Limits.Override = float64(*sc.MemoryBytes), true
			}
		}
		d.Limits.Pending = find(kindLimits, "limits")

		for _, vm := range v.Store.Volumes(p.EnvID, p.ServiceID) {
			d.Volumes = append(d.Volumes, VolumeView{
				ID: vm.VolumeID, MountPath: vm.MountPath, SizeMB: vm.SizeMB, UsedMB: vm.UsedMB,
				Pending: find(kindVolume, volumeTarget+vm.VolumeID),
			})
		}

		d.Domains = v.domains(p, pend)
		d.Charts = v.charts(p, step, span)
		return d
	})
}

func (v *Views) domains(p domain.Place, pend []changeset.Change) []DomainView {
	var out []DomainView
	listed := map[string]bool{}
	// change finds the pending change of a kind and target at p
	change := func(kind, target string) (*apply.Overlay, string) {
		for _, c := range pend {
			if c.Kind == kind && strings.EqualFold(c.Target, target) {
				return v.overlay(c), c.ID
			}
		}
		return nil, ""
	}
	doms, _ := v.Store.Domains(p)
	for _, s := range doms.Service {
		dv := DomainView{Host: s.Domain, Port: s.Port, Verified: true, ID: s.ID}
		dv.Pending, dv.ChangeID = change(kindServiceDomDel, serviceDomainTgt+s.ID)
		out = append(out, dv)
	}
	for _, c := range doms.Custom {
		listed[strings.ToLower(c.Domain)] = true
		dv := DomainView{Host: c.Domain, Port: c.Port, Custom: true, Verified: c.Verified, ID: c.ID, CertStatus: c.CertStatus}
		dv.Pending, dv.ChangeID = change(kindCustomDomDel, customDomainTarget+c.Domain)
		out = append(out, dv)
	}
	for _, c := range pend {
		switch c.Kind {
		case kindServiceDomAdd:
			out = append(out, DomainView{Port: newPort(c), Pending: v.overlay(c), ChangeID: c.ID})
		case kindCustomDomAdd:
			host, _ := strings.CutPrefix(c.Target, customDomainTarget)
			if listed[strings.ToLower(host)] {
				continue
			}
			listed[strings.ToLower(host)] = true
			out = append(out, DomainView{Host: host, Port: newPort(c), Custom: true, Pending: v.overlay(c), ChangeID: c.ID})
		}
	}
	return out
}

// newPort is the port a pending domain add asks for (0: Railway's default,
// or unreadable).
func newPort(c changeset.Change) int {
	var n struct {
		Port int `json:"port"`
	}
	_ = json.Unmarshal(c.New, &n)
	return n.Port
}

func (v *Views) charts(p domain.Place, step int, span time.Duration) []ChartView {
	ps := func(m string) []railway.MetricPoint { return v.Store.PanelSeries(p, step, span, m) }
	chart := func(name string, vals []float64, limit float64) ChartView {
		c := ChartView{Name: name, Values: vals, Limit: limit, HasData: len(vals) > 0}
		if len(vals) > 0 {
			c.Last, c.Peak = vals[len(vals)-1], slices.Max(vals)
		}
		return c
	}
	net := chart("net", monitor.Rate(ps("NETWORK_RX_GB"), ps("NETWORK_TX_GB")), 0)
	net.Limit = net.Peak
	return []ChartView{
		chart("cpu", values(ps("CPU_USAGE")), newestValue(ps("CPU_LIMIT"))),
		chart("mem", values(ps("MEMORY_USAGE_GB")), newestValue(ps("MEMORY_LIMIT_GB"))),
		net,
	}
}
