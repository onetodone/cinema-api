package redis

import (
	"context"
	"fmt"
	"strconv"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/platform/metrics"
)

var (
	holdAcquire = script("hold_acquire.lua")
	holdExtend  = script("hold_extend.lua")
	holdRelease = script("hold_release.lua")
)

// HoldGate implements booking.HoldGate: one key per seat of a showtime, whose value is the id of the booking
// that holds or claims the seat. It lets the booking service turn away requests for seats that another booking
// holds before they take a database connection and queue on a row lock. It decides nothing on its own: a request
// that passes still has to win the row locks in PostgreSQL.
//
// Keys are named <prefix>:hold:{<showtimeID>}:<seatID>. The hash tag {<showtimeID>} puts all seats of a showtime
// into one Redis Cluster slot, which the scripts need, because each touches the keys of several seats at once.
type HoldGate struct {
	s *Store
}

// HoldGate returns the hold gate.
func (s *Store) HoldGate() *HoldGate {
	return &HoldGate{s: s}
}

func (g *HoldGate) keys(showtimeID int64, seatIDs []int64) []string {
	tag := "{" + strconv.FormatInt(showtimeID, 10) + "}"
	keys := make([]string, len(seatIDs))
	for i, id := range seatIDs {
		keys[i] = g.s.key("hold", tag, strconv.FormatInt(id, 10))
	}
	return keys
}

// Acquire claims the seats for token for ttl, all or nothing, and returns the seats that other tokens hold. If
// any seat is taken, nothing is claimed.
func (g *HoldGate) Acquire(ctx context.Context, showtimeID int64, seatIDs []int64, token uuid.UUID, ttl time.Duration) ([]int64, error) {
	if ttl < time.Millisecond {
		return nil, fmt.Errorf("claim seats: ttl must be at least 1ms, got %s", ttl)
	}
	positions, err := holdAcquire.Run(ctx, g.s.rdb, g.keys(showtimeID, seatIDs), token.String(), ttl.Milliseconds()).Int64Slice()
	if err != nil {
		g.s.failed(ctx, metrics.OpHoldAcquire, err)
		return nil, fmt.Errorf("claim seats %v of showtime %d: %w", seatIDs, showtimeID, err)
	}

	conflicts := make([]int64, len(positions))
	for i, pos := range positions {
		conflicts[i] = seatIDs[pos-1] // Lua counts from 1
	}
	if len(conflicts) > 0 {
		g.s.metrics.HoldGateRejections.Inc()
	}
	return conflicts, nil
}

// ExtendUntil makes the seats that token holds expire at until. Seats that other tokens hold, or that nobody
// holds any more, are left alone. An until in the past releases the seats.
func (g *HoldGate) ExtendUntil(ctx context.Context, showtimeID int64, seatIDs []int64, token uuid.UUID, until time.Time) error {
	err := holdExtend.Run(ctx, g.s.rdb, g.keys(showtimeID, seatIDs), token.String(), until.UnixMilli()).Err()
	if err != nil {
		g.s.failed(ctx, metrics.OpHoldExtend, err)
		return fmt.Errorf("extend the hold of seats %v of showtime %d: %w", seatIDs, showtimeID, err)
	}
	return nil
}

// Release frees the seats that token holds. Seats that other tokens hold are left alone.
func (g *HoldGate) Release(ctx context.Context, showtimeID int64, seatIDs []int64, token uuid.UUID) error {
	err := holdRelease.Run(ctx, g.s.rdb, g.keys(showtimeID, seatIDs), token.String()).Err()
	if err != nil {
		g.s.failed(ctx, metrics.OpHoldRelease, err)
		return fmt.Errorf("release the hold of seats %v of showtime %d: %w", seatIDs, showtimeID, err)
	}
	return nil
}
