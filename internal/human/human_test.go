package human

import (
	"testing"
	"time"
)

func TestBytesReadsLikeTheRestOfTheServer(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1024, "1.0 KiB"},
		{8 << 30, "8.0 GiB"},
		{-(4 << 30), "-4.0 GiB"},
	} {
		if got := Bytes(tc.n); got != tc.want {
			t.Errorf("Bytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestDurationStopsAtOneUsefulUnit(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{38 * time.Second, "38s"},
		{90 * time.Second, "1m 30s"},
		{74 * time.Minute, "1h 14m"},
		{50 * time.Hour, "2 days"},
	} {
		if got := Duration(tc.d); got != tc.want {
			t.Errorf("Duration(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// A library nothing has ever been done to is the expected state here, not a
// gap in the data, so it has to read as a real answer.
func TestAgoAndUntilHandleTheTimesThatAreNotSet(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	if got := Ago(time.Time{}, now); got != "never" {
		t.Errorf("Ago(zero) = %q, want %q", got, "never")
	}
	if got := Until(time.Time{}, now); got != "not scheduled" {
		t.Errorf("Until(zero) = %q, want %q", got, "not scheduled")
	}
	if got := Ago(now.Add(-2*time.Hour), now); got != "2h 00m ago" {
		t.Errorf("Ago = %q", got)
	}
	if got := Ago(now.Add(-time.Second), now); got != "just now" {
		t.Errorf("Ago(a second ago) = %q, want %q", got, "just now")
	}
	if got := Until(now.Add(90*time.Minute), now); got != "in 1h 30m" {
		t.Errorf("Until = %q", got)
	}
	// A timer that has already come due should not read as a negative wait.
	if got := Until(now.Add(-time.Minute), now); got != "any moment" {
		t.Errorf("Until(past) = %q, want %q", got, "any moment")
	}
}
