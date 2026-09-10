// Package human formats numbers for people rather than for machines.
//
// It exists because the terminal reports and the web UI have to agree. A file
// that `status` calls "4.2 GiB" and the queue page calls "4509715660 bytes" is
// the same file described two ways, and the operator has to do the conversion
// in their head to know that.
package human

import (
	"fmt"
	"time"
)

// Bytes formats a size in binary units, which is what every other tool on this
// server (df, du, Unraid itself) reports in.
func Bytes(n int64) string {
	const unit = 1024
	neg := ""
	if n < 0 {
		neg, n = "-", -n
	}
	if n < unit {
		return fmt.Sprintf("%s%d B", neg, n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%s%.1f %ciB", neg, float64(n)/float64(div), "KMGTP"[exp])
}

// Duration is a length of time at one significant unit, which is all anyone
// wants from "how long did that encode take". Go's own formatting would say
// "1h14m22.318s"; here that is "1h 14m".
func Duration(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%.0f days", d.Hours()/24)
}

// Ago describes when something happened relative to now. A zero time reads as
// "never", because on this project that is a real and common answer -- a
// library nothing has ever been done to is the expected state, not a gap.
func Ago(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	if d < 0 {
		return "in " + Duration(-d)
	}
	if d < 45*time.Second {
		return "just now"
	}
	return Duration(d) + " ago"
}

// Until is the mirror of Ago, for a time in the future -- the next scan, or
// when a file that failed will be tried again.
func Until(t, now time.Time) string {
	if t.IsZero() {
		return "not scheduled"
	}
	d := t.Sub(now)
	if d <= 0 {
		return "any moment"
	}
	return "in " + Duration(d)
}
