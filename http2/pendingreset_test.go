package http2

import "testing"

// The reset back-pressure counter has to stay strictly below the peer's
// concurrency limit, or a peer that answers nothing wedges the connection.
//
// It feeds currentRequestCountLocked, which is compared against
// maxConcurrentStreams, and the slot wait parks on a condition variable with
// no timeout. Nothing clears the counter except a frame from the peer: the
// PING whose ack used to clear it is gone, and so is the health check that
// used to tear such a connection down.
//
// A flat ceiling is not a bound. Against a peer advertising fewer streams than
// the ceiling, resets alone reach saturation and every later request blocks to
// its own deadline.
func TestPendingResetLimitLeavesRoomToProgress(t *testing.T) {
	for _, tc := range []struct {
		maxConcurrent uint32
		want          int
	}{
		{0, 0},   // nothing advertised yet
		{1, 0},   // one stream at a time: no back-pressure rather than a wedge
		{2, 1},
		{4, 2},
		{8, 4},
		{100, 32},     // the initial default, clamped by the ceiling
		{1000, 32},    // a generous peer, still clamped
		{1 << 31, 32}, // "infinite"
	} {
		if got := pendingResetLimit(tc.maxConcurrent); got != tc.want {
			t.Errorf("pendingResetLimit(%d) = %d, want %d", tc.maxConcurrent, got, tc.want)
		}
		// The property that matters, stated independently of the table: the
		// counter can never reach the peer's limit on its own.
		if tc.maxConcurrent > 0 && pendingResetLimit(tc.maxConcurrent) >= int(tc.maxConcurrent) {
			t.Errorf("pendingResetLimit(%d) = %d, which saturates the connection "+
				"on resets alone", tc.maxConcurrent, pendingResetLimit(tc.maxConcurrent))
		}
	}
}
