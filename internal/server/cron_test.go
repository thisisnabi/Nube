package server

import (
	"testing"
	"time"
)

func TestCronNext(t *testing.T) {
	base := time.Date(2026, 10, 23, 10, 7, 30, 0, time.UTC) // a Friday
	cases := []struct {
		expr, want string
	}{
		{"*/15 * * * *", "2026-10-23 10:15"},
		{"0 * * * *", "2026-10-23 11:00"},
		{"@daily", "2026-10-24 00:00"},
		{"30 9 * * mon-fri", "2026-10-26 09:30"},
		{"0 0 1 jan *", "2027-01-01 00:00"},
		{"0 12 15 * 1", "2026-10-26 12:00"}, // dom OR dow
		{"5,10 10 * * *", "2026-10-23 10:10"},
		{"@every 5m", "2026-10-23 10:12"},
		{"0 0 * * 7", "2026-10-25 00:00"}, // 7 = Sunday
	}
	for _, c := range cases {
		s, err := parseSchedule(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := s.Next(base).Format("2006-01-02 15:04"); got != c.want {
			t.Errorf("%s: next = %s, want %s", c.expr, got, c.want)
		}
	}
	for _, bad := range []string{"* * *", "61 * * * *", "*/0 * * * *", "@every 10s"} {
		if _, err := parseSchedule(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
