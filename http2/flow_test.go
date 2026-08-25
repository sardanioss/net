// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"testing"
	"time"
)

// t0 is a fixed instant so the timed arm is driven explicitly rather than by
// the wall clock.
var t0 = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

func TestInFlowTake(t *testing.T) {
	var f inflow
	f.init(100, t0)
	if !f.take(40) {
		t.Fatalf("f.take(40) from 100: got false, want true")
	}
	if !f.take(40) {
		t.Fatalf("f.take(40) from 60: got false, want true")
	}
	if f.take(40) {
		t.Fatalf("f.take(40) from 20: got true, want false")
	}
	if !f.take(20) {
		t.Fatalf("f.take(20) from 20: got false, want true")
	}
}

func TestInflowAddSmall(t *testing.T) {
	var f inflow
	f.init(0, t0)
	// A zero window has a zero threshold, so any amount is strictly above it.
	if got, want := f.add(1, t0), int32(1); got != want {
		t.Fatalf("f.add(1) to 0 = %v, want %v", got, want)
	}
}

// A zero-byte add must be a complete no-op. The DATA frame handler calls
// add(refund) unconditionally and the refund is zero on every unpadded frame,
// so if a zero add touched lastUpdate the timed arm could never fire on a
// connection that is receiving data.
func TestInflowAddZeroDoesNotArmTheClock(t *testing.T) {
	var f inflow
	f.init(1000, t0)
	if got := f.add(400, t0); got != 0 {
		t.Fatalf("f.add(400) = %v, want 0 (400 <= 500)", got)
	}
	// Six seconds later, but every intervening event was a zero refund.
	for i := 1; i <= 6; i++ {
		if got := f.add(0, t0.Add(time.Duration(i)*time.Second)); got != 0 {
			t.Fatalf("f.add(0) at +%ds = %v, want 0", i, got)
		}
	}
	if got, want := f.add(1, t0.Add(6*time.Second)), int32(401); got != want {
		t.Fatalf("f.add(1) at +6s = %v, want %v (the interval must have elapsed)", got, want)
	}
}

func TestInflowAddProportional(t *testing.T) {
	var f inflow
	f.init(6291456, t0) // minRefresh = 3145728

	if got := f.add(16384, t0); got != 0 {
		t.Fatalf("f.add(16384) with a 6MB window = %v, want 0 (should buffer)", got)
	}

	// Exactly half buffers: the send arm is strictly greater than half.
	// Modelled properly, with the peer spending the window as it sends and the
	// delegate returning it as it consumes.
	var f2 inflow
	f2.init(6291456, t0)
	if !f2.take(3145728) {
		t.Fatal("f.take(half) failed")
	}
	if got := f2.add(3145728, t0); got != 0 {
		t.Fatalf("f.add(half) = %v, want 0; the threshold is strictly greater than half", got)
	}
	// One more byte crosses it and flushes everything accumulated.
	if !f2.take(1) {
		t.Fatal("f.take(1) failed")
	}
	if got, want := f2.add(1, t0), int32(3145729); got != want {
		t.Fatalf("f.add(1) past half = %v, want %v", got, want)
	}
	if f2.unsent != 0 {
		t.Fatalf("unsent = %d after a send, want 0", f2.unsent)
	}
	if got, want := f2.avail, int32(6291456); got != want {
		t.Fatalf("avail = %d after a send, want %d (the window is whole again)", got, want)
	}
}

// The old model floored the threshold at 4KB and carried a "this update at
// least doubles the peer's window" clause. Both are gone: for any window under
// 8KB the floor exceeded the whole window, so the doubling clause was the only
// thing that could ever fire, and removing one while keeping the other stalls
// a small window until the interval expires on every cycle.
func TestInflowSmallWindowHasNoFloor(t *testing.T) {
	var f inflow
	f.init(100, t0) // minRefresh = 50
	if got := f.add(50, t0); got != 0 {
		t.Fatalf("f.add(50) with a 100 window = %v, want 0 (exactly half buffers)", got)
	}
	if got, want := f.add(1, t0), int32(51); got != want {
		t.Fatalf("f.add(1) past half = %v, want %v", got, want)
	}
}

// The timed arm: an update below the threshold goes out anyway once the
// buffering interval has elapsed, so a slowly-consuming client does not look
// idle to the server. Chromium calls this
// kDefaultTimeToBufferSmallWindowUpdates and sets it to 5 seconds.
func TestInflowTimedArm(t *testing.T) {
	var f inflow
	f.init(1<<20, t0) // minRefresh = 524288, far above anything added here

	if got := f.add(1024, t0.Add(time.Second)); got != 0 {
		t.Fatalf("f.add(1024) at +1s = %v, want 0", got)
	}
	if got := f.add(1024, t0.Add(4*time.Second)); got != 0 {
		t.Fatalf("f.add(1024) at +4s = %v, want 0", got)
	}
	if got, want := f.add(1024, t0.Add(5*time.Second)), int32(3072); got != want {
		t.Fatalf("f.add(1024) at +5s = %v, want %v", got, want)
	}
	// The clock resets only when a frame actually goes out, so the next
	// interval is measured from +5s and not from the last add.
	if got := f.add(1024, t0.Add(9*time.Second)); got != 0 {
		t.Fatalf("f.add(1024) at +9s = %v, want 0 (only 4s since the last send)", got)
	}
	if got, want := f.add(1024, t0.Add(10*time.Second)), int32(2048); got != want {
		t.Fatalf("f.add(1024) at +10s = %v, want %v", got, want)
	}
}

func TestTakeInflows(t *testing.T) {
	var a, b inflow
	a.init(10, t0)
	b.init(20, t0)
	if !takeInflows(&a, &b, 5) {
		t.Fatalf("takeInflows(a, b, 5) from 10, 20: got false, want true")
	}
	if takeInflows(&a, &b, 6) {
		t.Fatalf("takeInflows(a, b, 6) from 5, 15: got true, want false")
	}
	if !takeInflows(&a, &b, 5) {
		t.Fatalf("takeInflows(a, b, 5) from 5, 15: got false, want true")
	}
}

func TestOutFlow(t *testing.T) {
	var st outflow
	var conn outflow
	st.add(3)
	conn.add(2)

	if got, want := st.available(), int32(3); got != want {
		t.Errorf("available = %d; want %d", got, want)
	}
	st.setConnFlow(&conn)
	if got, want := st.available(), int32(2); got != want {
		t.Errorf("after parent setup, available = %d; want %d", got, want)
	}

	st.take(2)
	if got, want := conn.available(), int32(0); got != want {
		t.Errorf("after taking 2, conn = %d; want %d", got, want)
	}
	if got, want := st.available(), int32(0); got != want {
		t.Errorf("after taking 2, stream = %d; want %d", got, want)
	}
}

func TestOutFlowAdd(t *testing.T) {
	var f outflow
	if !f.add(1) {
		t.Fatal("failed to add 1")
	}
	if !f.add(-1) {
		t.Fatal("failed to add -1")
	}
	if got, want := f.available(), int32(0); got != want {
		t.Fatalf("size = %d; want %d", got, want)
	}
	if !f.add(1<<31 - 1) {
		t.Fatal("failed to add 2^31-1")
	}
	if got, want := f.available(), int32(1<<31-1); got != want {
		t.Fatalf("size = %d; want %d", got, want)
	}
	if f.add(1) {
		t.Fatal("adding 1 to max shouldn't be allowed")
	}
}

func TestOutFlowAddOverflow(t *testing.T) {
	var f outflow
	if !f.add(0) {
		t.Fatal("failed to add 0")
	}
	if !f.add(-1) {
		t.Fatal("failed to add -1")
	}
	if !f.add(0) {
		t.Fatal("failed to add 0")
	}
	if !f.add(1) {
		t.Fatal("failed to add 1")
	}
	if !f.add(1) {
		t.Fatal("failed to add 1")
	}
	if !f.add(0) {
		t.Fatal("failed to add 0")
	}
	if !f.add(-3) {
		t.Fatal("failed to add -3")
	}
	if got, want := f.available(), int32(-2); got != want {
		t.Fatalf("size = %d; want %d", got, want)
	}
	if !f.add(1<<31 - 1) {
		t.Fatal("failed to add 2^31-1")
	}
	if got, want := f.available(), int32(1+-3+(1<<31-1)); got != want {
		t.Fatalf("size = %d; want %d", got, want)
	}

}
