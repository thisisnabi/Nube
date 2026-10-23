package server

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// schedule is a parsed cron expression: "min hour day-of-month month day-of-week",
// with *, lists (1,5), ranges (1-5), steps (*/15, 0-30/5), month/day names,
// and the macros @yearly @monthly @weekly @daily @hourly and @every <duration>.
type schedule struct {
	minute, hour, dom, month, dow uint64 // bitsets
	domStar, dowStar              bool
	every                         time.Duration
}

var cronMacros = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
	"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

var monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
var dowNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

func parseSchedule(expr string) (*schedule, error) {
	expr = strings.TrimSpace(expr)
	if d, ok := strings.CutPrefix(expr, "@every "); ok {
		dur, err := time.ParseDuration(strings.TrimSpace(d))
		if err != nil || dur < time.Minute {
			return nil, fmt.Errorf("@every needs a duration of at least 1m")
		}
		return &schedule{every: dur}, nil
	}
	if m, ok := cronMacros[strings.ToLower(expr)]; ok {
		expr = m
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron schedule needs 5 fields (minute hour day month weekday), got %q", expr)
	}
	s := &schedule{domStar: f[2] == "*" || f[2] == "?", dowStar: f[4] == "*" || f[4] == "?"}
	var err error
	if s.minute, err = parseField(f[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}
	if s.hour, err = parseField(f[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}
	if s.dom, err = parseField(f[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("day of month: %w", err)
	}
	if s.month, err = parseField(f[3], 1, 12, monthNames); err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}
	if s.dow, err = parseField(f[4], 0, 7, dowNames); err != nil {
		return nil, fmt.Errorf("day of week: %w", err)
	}
	if s.dow&(1<<7) != 0 { // 7 = Sunday too
		s.dow |= 1
	}
	return s, nil
}

func parseField(field string, lo, hi int, names map[string]int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(field, ",") {
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("bad step %q", stepStr)
			}
			step = n
		}
		start, end := lo, hi
		if rng != "*" && rng != "?" {
			a, b, isRange := strings.Cut(rng, "-")
			var err error
			if start, err = cronValue(a, names); err != nil {
				return 0, err
			}
			end = start
			if isRange {
				if end, err = cronValue(b, names); err != nil {
					return 0, err
				}
			} else if hasStep {
				end = hi
			}
		}
		if start < lo || end > hi || start > end {
			return 0, fmt.Errorf("%q out of range %d-%d", part, lo, hi)
		}
		for v := start; v <= end; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, nil
}

func cronValue(s string, names map[string]int) (int, error) {
	if v, ok := names[strings.ToLower(s)]; ok {
		return v, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("bad value %q", s)
	}
	return v, nil
}

func (s *schedule) dayMatches(t time.Time) bool {
	dom := s.dom&(1<<uint(t.Day())) != 0
	dow := s.dow&(1<<uint(t.Weekday())) != 0
	if s.domStar || s.dowStar {
		return dom && dow
	}
	return dom || dow // classic cron: either restricted field may match
}

// Next returns the first activation strictly after t (in t's location).
func (s *schedule) Next(t time.Time) time.Time {
	if s.every > 0 {
		return t.Truncate(time.Minute).Add(s.every)
	}
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if s.month&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
			continue
		}
		if s.hour&(1<<uint(t.Hour())) == 0 {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
			continue
		}
		if s.minute&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{} // e.g. "0 0 30 2 *" never fires
}
