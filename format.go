package main

import (
	"errors"
	"fmt"
	"time"
)

func asIndexing(err error, target **IndexingError) bool { return errors.As(err, target) }

func formatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// formatTime shows fsearch's modification time (seconds since the epoch).
func formatTime(sec int64) string {
	if sec <= 0 {
		return ""
	}
	t := time.Unix(sec, 0)
	if time.Since(t) < 180*24*time.Hour {
		return t.Format("Jan _2 15:04")
	}
	return t.Format("Jan _2  2006")
}

func formatDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%d µs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%d ms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1f s", d.Seconds())
}

func formatCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}
