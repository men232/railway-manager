package railway

import "time"

// BuildDiscoveryRequestForTest exposes the request builder to external tests.
func BuildDiscoveryRequestForTest(t DiscoveryTarget) (string, map[string]any) {
	r := buildDiscoveryRequest(t)
	return r.document, r.variables
}

// LiveTimings shortens a Live's clocks in tests; zero fields keep the default.
type LiveTimings struct {
	AckWait, Grace, Ping, PongWait, Stable, CloseWait time.Duration
}

// SetLiveTimings must be called before the first Subscribe.
func SetLiveTimings(l *Live, t LiveTimings) {
	set := func(dst *time.Duration, v time.Duration) {
		if v != 0 {
			*dst = v
		}
	}
	set(&l.timing.ackWait, t.AckWait)
	set(&l.timing.grace, t.Grace)
	set(&l.timing.ping, t.Ping)
	set(&l.timing.pongWait, t.PongWait)
	set(&l.timing.stable, t.Stable)
	set(&l.timing.closeWait, t.CloseWait)
}
