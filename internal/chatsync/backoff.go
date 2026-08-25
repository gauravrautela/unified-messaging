package chatsync

import (
	"math/rand"
	"time"
)

const (
	// maxFailures is how many consecutive failed attempts an account gets
	// before we stop guessing and hand it back to the end user. At the capped
	// interval that is a little over two hours of trying.
	maxFailures = 30

	// stableAfter is how long a connection must hold before the failure
	// counter is forgiven, so a socket that flaps once a day never walks its
	// way to the cap.
	stableAfter = 10 * time.Minute

	maxBackoff = 5 * time.Minute
)

// next returns the wait before reconnect attempt n (0-based): 1s, 2s, 4s …
// capped at 5m, with ±20% jitter so a fleet of accounts does not reconnect
// in lockstep.
func next(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	// Shifting past 9 would overflow long before it mattered; the cap below
	// makes anything beyond that identical anyway.
	d := time.Second << uint(min(attempt, 9))
	if d > maxBackoff {
		d = maxBackoff
	}
	j := 1 + (rand.Float64()*0.4 - 0.2)
	return time.Duration(float64(d) * j)
}
