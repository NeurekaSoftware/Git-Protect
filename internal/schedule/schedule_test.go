package schedule

import (
	"errors"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	valid := []string{
		"0 */6 * * *",
		"*/15 * * * *",
		"0 3 * * *",
		"0 0 * * 0",
		"30 4 1,15 * *",
		"0 12 * * MON",
		"0 0 ? * *",
		"0 0 * jan *",
		"30 0 1 * *",
		"0 30 2 * * *",
		"5/10 * * * *",
		"0 9-17 * * *",
	}
	for _, expression := range valid {
		if _, err := Parse(expression); err != nil {
			t.Errorf("Parse(%q) should accept: %v", expression, err)
		}
	}

	for _, expression := range []string{"", "   ", "not a cron", "* * * *", "* * * * * * *", "@hourly", "0 0 0 0 0"} {
		if _, err := Parse(expression); err == nil {
			t.Errorf("Parse(%q) should reject", expression)
		}
	}

	if _, err := Parse(""); !errors.Is(err, errCronRequired) {
		t.Errorf("blank expression error = %v, want %v", err, errCronRequired)
	}
	if _, err := Parse("nonsense"); !errors.Is(err, errCronInvalid) {
		t.Errorf("invalid expression error = %v, want %v", err, errCronInvalid)
	}
}

func TestParseErrorStrings(t *testing.T) {
	if _, err := Parse(" "); err == nil || err.Error() != "cron expression is required." {
		t.Fatalf("blank error = %v, want the required-expression string", err)
	}
	if _, err := Parse("99 99 99 99 99"); err == nil || err.Error() != "must be a valid 5-field or 6-field cron expression." {
		t.Fatalf("invalid error = %v, want the invalid-expression string", err)
	}
}

func TestNext(t *testing.T) {
	// Local timezone semantics: 04:30 local daily, evaluated from 03:00 local.
	location := time.FixedZone("TEST", 2*60*60)
	after := time.Date(2026, 9, 9, 3, 0, 0, 0, location)

	schedule, err := Parse("30 4 * * *")
	if err != nil {
		t.Fatal(err)
	}
	next := schedule.Next(after)
	if got := next.Format("2006-01-02 15:04:05"); got != "2026-09-09 04:30:00" {
		t.Fatalf("Next = %s, want 2026-09-09 04:30:00", got)
	}

	// Six-field expressions honor the seconds component.
	sixField, err := Parse("15 30 4 * * *")
	if err != nil {
		t.Fatal(err)
	}
	next = sixField.Next(after)
	if got := next.Format("15:04:05"); got != "04:30:15" {
		t.Fatalf("six-field Next = %s, want 04:30:15", got)
	}
}

func TestDurationShort(t *testing.T) {
	tests := []struct {
		seconds int64
		want    string
	}{
		{seconds: 0, want: "0s"},
		{seconds: -5, want: "0s"},
		{seconds: 1, want: "1s"},
		{seconds: 59, want: "59s"},
		{seconds: 60, want: "1m"},
		{seconds: 90, want: "1m 30s"},
		{seconds: 3600, want: "1h"},
		{seconds: 5400, want: "1h 30m"},
		{seconds: 3661, want: "1h 1m 1s"},
		{seconds: 86400, want: "1d"},
		{seconds: 604800, want: "1w"},
		{seconds: 2592000, want: "1mo"},
		{seconds: 31536000, want: "1y"},
		{seconds: 34822861, want: "1y 1mo 1w 1d 1h 1m 1s"},
	}
	for _, tt := range tests {
		if got := DurationShort(tt.seconds); got != tt.want {
			t.Errorf("DurationShort(%d) = %q, want %q", tt.seconds, got, tt.want)
		}
	}
}

func TestFormatTimestamp(t *testing.T) {
	// The announcement timestamp is rendered in the process's local zone, so
	// build the input there and assert the layout only.
	value := time.Date(2026, 9, 9, 14, 5, 9, 0, time.Local)
	if got, want := FormatTimestamp(value), "2026-09-09 14:05:09"; got != want {
		t.Fatalf("FormatTimestamp = %q, want %q", got, want)
	}
}
