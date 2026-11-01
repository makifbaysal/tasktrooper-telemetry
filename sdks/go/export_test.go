package trooper

import "time"

// SetFirstExportDelay overrides the early metric export delay for a test.
func SetFirstExportDelay(d time.Duration) (restore func()) {
	prev := firstExportDelay
	firstExportDelay = d
	return func() { firstExportDelay = prev }
}
