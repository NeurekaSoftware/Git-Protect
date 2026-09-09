// Package schedule parses cron expressions and computes their next occurrence
// in the machine's local timezone.
package schedule

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// The parse errors double as the settings validator's user-facing strings, so
// they carry the exact wording the daemon has always reported.
var (
	errCronRequired = errors.New("cron expression is required.")
	errCronInvalid  = errors.New("must be a valid 5-field or 6-field cron expression.")
)

// parser accepts standard 5-field expressions and 6-field expressions with a
// leading seconds field. Descriptors like @hourly are deliberately not in the
// accepted set.
var parser = cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// Schedule evaluates a parsed cron expression.
type Schedule struct {
	spec cron.Schedule
}

// Parse validates a cron expression, accepting 5 fields or 6 fields with
// optional seconds. The returned error text is one of the fixed validation
// strings consumed by the settings loader.
func Parse(expression string) (Schedule, error) {
	if strings.TrimSpace(expression) == "" {
		return Schedule{}, errCronRequired
	}

	spec, err := parser.Parse(expression)
	if err != nil {
		return Schedule{}, errCronInvalid
	}
	return Schedule{spec: spec}, nil
}

// Next returns the next occurrence strictly after after, evaluated in the
// local timezone so DST transitions behave like the wall clock.
func (s Schedule) Next(after time.Time) time.Time {
	return s.spec.Next(after)
}

// FormatTimestamp renders a local timestamp for scheduler announcements.
func FormatTimestamp(value time.Time) string {
	return value.Local().Format("2006-01-02 15:04:05")
}

// DurationShort renders a raw second count as compact space-separated parts,
// e.g. "1h 30m". Fixed unit sizes keep the output deterministic for raw second
// counts, and zero or negative durations render as "0s".
func DurationShort(totalSeconds int64) string {
	remaining := max(totalSeconds, 0)
	var parts []string
	for _, unit := range durationUnits {
		if remaining < unit.seconds {
			continue
		}
		value := remaining / unit.seconds
		parts = append(parts, strconv.FormatInt(value, 10)+unit.suffix)
		remaining -= value * unit.seconds
	}
	if len(parts) == 0 {
		return "0s"
	}
	return strings.Join(parts, " ")
}

type durationUnit struct {
	seconds int64
	suffix  string
}

var durationUnits = []durationUnit{
	{365 * 24 * 60 * 60, "y"},
	{30 * 24 * 60 * 60, "mo"},
	{7 * 24 * 60 * 60, "w"},
	{24 * 60 * 60, "d"},
	{60 * 60, "h"},
	{60, "m"},
	{1, "s"},
}
