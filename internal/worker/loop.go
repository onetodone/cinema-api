// Package worker runs the background jobs of cmd/worker. A job sweeps once at start and then again after
// every jittered interval, until its context is canceled.
package worker

import (
	"context"
	"math/rand/v2"
	"time"
)

// maxJitterPercent is how far one pause between two sweeps may stray from the interval, in either direction.
// Replicas that started at the same moment drift apart instead of querying the database in lockstep.
const maxJitterPercent = 20

// every calls sweep at once and then again whenever a jittered interval has passed since the previous call
// returned, until ctx is canceled. Calls never overlap: a slow sweep delays the next one instead of piling up.
func every(ctx context.Context, interval time.Duration, sweep func(ctx context.Context)) {
	for ctx.Err() == nil {
		sweep(ctx)

		timer := time.NewTimer(jittered(interval))
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

// jittered returns d changed at random by up to maxJitterPercent in either direction.
func jittered(d time.Duration) time.Duration {
	spread := d * maxJitterPercent / 100
	if spread <= 0 {
		return d
	}
	return d - spread + rand.N(2*spread+1) //nolint:gosec // G404: jitter needs no cryptographic randomness
}
