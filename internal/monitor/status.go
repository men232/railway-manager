// Package monitor holds the pure calculations behind the Services screen:
// status, braille charts, traffic rates, column layout and number formats.
package monitor

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/men232/railway-manager/internal/railway"
)

// Kind is what the services screen says about a service.
type Kind int

const (
	Reading Kind = iota
	Online
	OnlineDeploying
	Deploying
	Failed
	Offline
	Sleeping
)

// Status is a Kind with its glyph and text. Icon is the standard-Unicode
// glyph (the TUI maps it to a Nerd form when wanted); Label is Icon + " " +
// Text, or just Text-like "… reading" before the first read.
type Status struct {
	Kind       Kind
	Icon, Text string
	Label      string
}

func mk(k Kind, icon, text string) Status {
	return Status{Kind: k, Icon: icon, Text: text, Label: icon + " " + text}
}

var statuses = map[Kind]Status{
	Reading:         mk(Reading, "…", "reading"),
	Online:          mk(Online, "●", "ONLINE"),
	OnlineDeploying: mk(OnlineDeploying, "◐", "ONLINE / DEPLOYING"),
	Deploying:       mk(Deploying, "◌", "DEPLOYING"),
	Failed:          mk(Failed, "✗", "OFFLINE / FAILED"),
	Offline:         mk(Offline, "○", "OFFLINE"),
	Sleeping:        mk(Sleeping, "☾", "SLEEPING"),
}

func dead(s string) bool { return s == "FAILED" || s == "CRASHED" || s == "REMOVED" }

// StatusOf is the state of one service instance, from its deployment states
// alone. read says the deployment states have been read at all (false:
// "… reading"). A running deployment that Railway has put to sleep has status
// SLEEPING; a service's "sleeps when idle" setting says nothing about now. A
// newer failed deployment leaves the running one online, as Railway does.
func StatusOf(st railway.DeploymentState, read bool) Status {
	if !read {
		return statuses[Reading]
	}
	active := st.ActiveStatus
	inProgress := len(st.InProgress) > 0
	switch {
	case active == "SLEEPING":
		return statuses[Sleeping]
	case dead(active):
		return statuses[Failed]
	case active != "":
		if inProgress {
			return statuses[OnlineDeploying]
		}
		return statuses[Online]
	case inProgress:
		return statuses[Deploying]
	case dead(st.LatestStatus):
		return statuses[Failed]
	}
	return statuses[Offline]
}

// GB is Railway's gigabyte, 1024³ bytes: metrics in GB, memory limits and
// their overrides are all in it (volume sizes are decimal MB, not this).
const GB = 1024 * 1024 * 1024

// Rate is bytes/s per interval of two series in GB: rx and tx are summed per
// timestamp over the union of their timestamps (a direction missing at a stamp
// counts 0), sorted by time; each value is moved * 1024³ / gap seconds, the
// gap being 30 s (the sample rate) for the first stamp, else the time since
// the previous stamp. A non-positive gap gives 0.
func Rate(rx, tx []railway.MetricPoint) []float64 {
	moved := map[int64]float64{}
	for _, p := range rx {
		moved[p.TS] += p.Value
	}
	for _, p := range tx {
		moved[p.TS] += p.Value
	}
	stamps := make([]int64, 0, len(moved))
	for ts := range moved {
		stamps = append(stamps, ts)
	}
	slices.Sort(stamps)
	out := make([]float64, 0, len(stamps))
	for i, ts := range stamps {
		gap := int64(30)
		if i > 0 {
			gap = ts - stamps[i-1]
		}
		out = append(out, perSecond(moved[ts], gap))
	}
	return out
}

func perSecond(gbMoved float64, gap int64) float64 {
	if gap <= 0 {
		return 0
	}
	return gbMoved * GB / float64(gap)
}

// NewestRates is the newest point of each direction as a rate over the gap to
// its own previous point (30 s when it has none). ok is false when both
// series are empty; an empty direction reads 0.
func NewestRates(rx, tx []railway.MetricPoint) (rxBps, txBps float64, ok bool) {
	return newest(rx), newest(tx), len(rx) > 0 || len(tx) > 0
}

func newest(pts []railway.MetricPoint) float64 {
	if len(pts) == 0 {
		return 0
	}
	sorted := slices.SortedFunc(slices.Values(pts), func(a, b railway.MetricPoint) int { return cmp.Compare(a.TS, b.TS) })
	last := sorted[len(sorted)-1]
	gap := int64(30)
	if len(sorted) > 1 {
		gap = last.TS - sorted[len(sorted)-2].TS
	}
	return perSecond(last.Value, gap)
}

func format(v float64, units []string, suffix string) string {
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	// 1023.96 would print as 1024.0: say it in the next unit.
	if i < len(units)-1 && fmt.Sprintf("%.1f", v) == "1024.0" {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s%s", v, units[i], suffix)
}

// FormatBytes prints bytes with one decimal: 1023.9 MB, 1.0 GB.
func FormatBytes(b float64) string { return format(b, []string{"B", "KB", "MB", "GB"}, "") }

// FormatRate prints bytes per second: 1023.9 KB/s.
func FormatRate(bps float64) string { return format(bps, []string{"B", "KB", "MB", "GB"}, "/s") }

// FormatCPU prints vCPU with two decimals.
func FormatCPU(v float64) string { return fmt.Sprintf("%.2f", v) }
