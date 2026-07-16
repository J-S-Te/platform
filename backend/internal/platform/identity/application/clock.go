package application

import "time"

// SystemClock returns UTC wall-clock time for production use.
type SystemClock struct{}

// Now returns the current UTC time.
func (SystemClock) Now() time.Time {
	return time.Now().UTC()
}
