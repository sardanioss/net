// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Flow control

package http2

import "time"

// inflowSmallUpdateInterval is how long an update below the proportional
// threshold is buffered before it goes out anyway.
//
// Chromium's rationale, verbatim from net/spdy/spdy_session.h next to
// kDefaultTimeToBufferSmallWindowUpdates: "Usually window updates are sent
// when half of the receive window has been processed by the client but in the
// case of a client that consumes the data slowly, this strategy alone would
// make servers consider the connection or stream idle."
const inflowSmallUpdateInterval = 5 * time.Second

// inflow accounts for an inbound flow control window.
// It tracks both the latest window sent to the peer (used for enforcement)
// and the accumulated unsent window.
type inflow struct {
	avail      int32
	unsent     int32
	minRefresh int32     // half the full window; set by init
	lastUpdate time.Time // instant of the last update actually sent
}

// init sets the initial window.
func (f *inflow) init(n int32, now time.Time) {
	f.avail = n
	// Half the window, with no floor. The floor and the old "this update at
	// least doubles the peer's window" clause were load-bearing for each
	// other: for any window under twice the floor the threshold exceeded the
	// whole window, and the doubling clause was the only thing that could ever
	// fire. Removing one and keeping the other deadlocks a small window until
	// the interval expires, every cycle.
	f.minRefresh = n / 2
	f.lastUpdate = now
}

// add adds n bytes to the window, with a maximum window size of max,
// indicating that the peer can now send us more data.
// For example, the user read from a {Request,Response} body and consumed
// some of the buffered data, so the peer can now send more.
// It returns the number of bytes to send in a WINDOW_UPDATE frame to the peer.
//
// The predicate is the one in Chromium's SpdyStream::IncreaseRecvWindowSize:
// send once the unacknowledged count passes half the full window, or once the
// buffering interval has elapsed since the last update, and reset the clock
// only when a frame actually goes out.
func (f *inflow) add(n int, now time.Time) (connAdd int32) {
	if n < 0 {
		panic("negative update")
	}
	// Before the clock is consulted, not after. The DATA frame handler calls
	// add(refund) unconditionally and the refund is zero on every unpadded
	// frame, so touching lastUpdate first would let each arriving frame
	// restart the interval and the timed arm would never fire.
	if n == 0 {
		return 0
	}
	unsent := int64(f.unsent) + int64(n)
	// "A sender MUST NOT allow a flow-control window to exceed 2^31-1 octets."
	// RFC 7540 Section 6.9.1.
	const maxWindow = 1<<31 - 1
	if unsent+int64(f.avail) > maxWindow {
		panic("flow control update exceeds maximum window size")
	}
	f.unsent = int32(unsent)
	// Strictly greater than half sends, so exactly half buffers.
	if f.unsent <= f.minRefresh && now.Sub(f.lastUpdate) < inflowSmallUpdateInterval {
		return 0
	}
	f.lastUpdate = now
	f.avail += f.unsent
	f.unsent = 0
	return int32(unsent)
}

// take attempts to take n bytes from the peer's flow control window.
// It reports whether the window has available capacity.
func (f *inflow) take(n uint32) bool {
	if n > uint32(f.avail) {
		return false
	}
	f.avail -= int32(n)
	return true
}

// takeInflows attempts to take n bytes from two inflows,
// typically connection-level and stream-level flows.
// It reports whether both windows have available capacity.
func takeInflows(f1, f2 *inflow, n uint32) bool {
	if n > uint32(f1.avail) || n > uint32(f2.avail) {
		return false
	}
	f1.avail -= int32(n)
	f2.avail -= int32(n)
	return true
}

// outflow is the outbound flow control window's size.
type outflow struct {
	_ incomparable

	// n is the number of DATA bytes we're allowed to send.
	// An outflow is kept both on a conn and a per-stream.
	n int32

	// conn points to the shared connection-level outflow that is
	// shared by all streams on that conn. It is nil for the outflow
	// that's on the conn directly.
	conn *outflow
}

func (f *outflow) setConnFlow(cf *outflow) { f.conn = cf }

func (f *outflow) available() int32 {
	n := f.n
	if f.conn != nil && f.conn.n < n {
		n = f.conn.n
	}
	return n
}

func (f *outflow) take(n int32) {
	if n > f.available() {
		panic("internal error: took too much")
	}
	f.n -= n
	if f.conn != nil {
		f.conn.n -= n
	}
}

// add adds n bytes (positive or negative) to the flow control window.
// It returns false if the sum would exceed 2^31-1.
func (f *outflow) add(n int32) bool {
	sum := f.n + n
	if (sum > n) == (f.n > 0) {
		f.n = sum
		return true
	}
	return false
}
