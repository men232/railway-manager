package rotate

import (
	"testing"
	"time"
)

func TestAgo(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "now"},
		{5 * time.Minute, "5 min ago"},
		{3 * time.Hour, "3h ago"},
		{30 * time.Hour, "1 day ago"},
		{212 * 24 * time.Hour, "212 days ago"},
	} {
		if got := ago(now, now.Add(-tc.d)); got != tc.want {
			t.Errorf("ago(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
