// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Flow control

package http2

import (
	"fmt"
	"time"
)

// inflowSmallUpdateInterval is how long an update below the proportional
// threshold is buffered before it goes out anyway.
//
// Chromium's rationale, verbatim from net/spdy/spdy_session.h next to
// kDefaultTimeToBufferSmallWindowUpdates: "Usually window updates are sent
// when half of the receive window has been processed by the client but in the
// case of a client that consumes the data slowly, this strategy alone would
// make servers consider the connection or stream idle."
const inflowSmallUpdateInterval = 5 * time.Second

// WindowRefresh is a rule for when a client hands receive credit back to the
// peer with WINDOW_UPDATE frames. One rule serves both scopes, the connection
// and each stream, each measured against its own window. See
// Transport.WindowRefresh, and the field comments for the values Firefox
// uses.
type WindowRefresh struct {
	// OnArrival counts a DATA frame's bytes, padding included, as soon as the
	// frame arrives, rather than when the response body is read. The
	// connection then hands back every byte it receives, whatever the reader
	// does. A stream holds back what its reader has left unread beyond
	// ReadBuffer, so a reader that stops still stops the peer. The frame
	// that ends a stream counts only for the connection. When both scopes
	// are due on one frame, the stream's update is written first and both go
	// out in one flush.
	//
	// That is Firefox's model (Http2Session.cpp). ReadyToProcessDataFrame
	// calls UpdateLocalRwin with the frame's length as each DATA frame
	// header is parsed. UpdateLocalRwin runs UpdateLocalStreamWindow, then
	// UpdateLocalSessionWindow, then flushes. The stream's count is
	// Http2StreamBase::LocalUnAcked, which subtracts the bytes the response
	// pipe could not take; the session's subtracts nothing. When parked bytes
	// drain, ProcessSlowConsumer runs the stream's check again with nothing
	// new, and so does a read here once it drains bytes held back that way.
	OnArrival bool

	// AckAt hands credit back once at least this many bytes are waiting. Zero
	// means once more than half the window is waiting, which is Chromium's
	// threshold. Firefox uses kMinimumToAck, 4194304.
	AckAt uint32

	// Floor also hands credit back once the window the peer has left has
	// fallen to this many bytes or fewer, however few are waiting. Zero
	// means no floor. Firefox uses kEmergencyWindowThreshold, 98304; below
	// it, in the words of Http2Session.h, "we ack every received byte".
	Floor uint32

	// Interval also hands credit back once this long has passed since the
	// last update. It is checked when credit is added, not on a timer of its
	// own. Zero means never. Chromium uses 5 seconds; Firefox has no timer.
	Interval time.Duration

	// ReadBuffer applies with OnArrival: how many unread bytes a stream may
	// hold and still hand back. Firefox's equivalent is the pipe between a
	// stream and its consumer, 24 segments of 32768 bytes, so 786432
	// (nsHttpTransaction's NS_NewPipe2; network.buffer.cache.count and
	// network.buffer.cache.size). Zero holds back every unread byte.
	ReadBuffer uint32
}

// checkReachable refuses a rule that can never hand credit back. With no
// Floor and no Interval, AckAt is the only trigger, and a window smaller than
// AckAt runs dry before that many bytes can be waiting, so the transfer
// stalls for good.
func (r *WindowRefresh) checkReachable(streamWindow, connWindow int32) error {
	if r.AckAt == 0 || r.Floor != 0 || r.Interval > 0 {
		return nil
	}
	if w := min(streamWindow, connWindow); int64(r.AckAt) > int64(w) {
		return fmt.Errorf("http2: WindowRefresh.AckAt %d is above the %d-byte receive window, and with no Floor or Interval credit would never go back", r.AckAt, w)
	}
	return nil
}

// due reports whether n waiting bytes go back to the peer now, given the
// window f still has and the time of its last update.
func (r *WindowRefresh) due(n int32, f *inflow, now time.Time) bool {
	if r.AckAt == 0 {
		if n > f.minRefresh {
			return true
		}
	} else if n >= int32(r.AckAt) {
		return true
	}
	if r.Floor != 0 && f.avail <= int32(r.Floor) {
		return true
	}
	return r.Interval > 0 && now.Sub(f.lastUpdate) >= r.Interval
}

// inflow accounts for an inbound flow control window.
// It tracks both the latest window sent to the peer (used for enforcement)
// and the accumulated unsent window.
type inflow struct {
	avail      int32
	unsent     int32
	minRefresh int32     // half the full window; set by init
	lastUpdate time.Time // instant of the last update actually sent

	// rule, when non-nil, decides when credit goes back instead of the
	// default below; see Transport.WindowRefresh. Only the client sets it.
	rule *WindowRefresh
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
	if f.rule != nil {
		return f.addHeld(n, 0, now)
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

// addHeld is add under f.rule, for a stream whose reader has left held of the
// waiting bytes unread beyond the rule's ReadBuffer. Those stay waiting. Unlike
// add, n may be zero: that checks the rule again after held has shrunk.
func (f *inflow) addHeld(n, held int, now time.Time) int32 {
	if n < 0 || held < 0 {
		panic("negative update")
	}
	unsent := int64(f.unsent) + int64(n)
	const maxWindow = 1<<31 - 1
	if unsent+int64(f.avail) > maxWindow {
		panic("flow control update exceeds maximum window size")
	}
	f.unsent = int32(unsent)
	send := unsent - int64(held)
	if send <= 0 || !f.rule.due(int32(send), f, now) {
		return 0
	}
	f.lastUpdate = now
	f.avail += int32(send)
	f.unsent -= int32(send)
	return int32(send)
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
