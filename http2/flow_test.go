// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import "testing"

func TestInFlowTake(t *testing.T) {
	var f inflow
	f.init(100)
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
	f.init(0)
	// Adding even a small amount when there is no flow causes an immediate send.
	if got, want := f.add(1), int32(1); got != want {
		t.Fatalf("f.add(1) to 0 = %v, want %v", got, want)
	}
}

func TestInflowAdd(t *testing.T) {
	var f inflow
	// With proportional threshold: init(40960) → minRefresh = 20480
	f.init(10 * inflowMinRefresh) // avail=40960, minRefresh=20480

	// Under threshold: 4095 < 20480 → buffer
	if got, want := f.add(inflowMinRefresh-1), int32(0); got != want {
		t.Fatalf("f.add(%d) = %v, want %v", inflowMinRefresh-1, got, want)
	}
	// Still under threshold: 4096 < 20480 → buffer
	if got, want := f.add(1), int32(0); got != want {
		t.Fatalf("f.add(1) at unsent=%d = %v, want %v", inflowMinRefresh, got, want)
	}
	// Cross threshold: add enough to reach minRefresh (20480)
	remaining := 20480 - inflowMinRefresh // 16384
	if got, want := f.add(remaining), int32(20480); got != want {
		t.Fatalf("f.add(%d) = %v, want %v", remaining, got, want)
	}
}

func TestInflowAddProportional(t *testing.T) {
	// Test with a large window (6MB, like Chrome)
	var f inflow
	f.init(6291456) // Chrome's INITIAL_WINDOW_SIZE
	// minRefresh = 6291456 / 2 = 3145728

	// Small reads should be buffered
	if got := f.add(16384); got != 0 {
		t.Fatalf("f.add(16384) with 6MB window = %v, want 0 (should buffer)", got)
	}

	// Keep adding until we cross the ~3MB threshold
	total := int32(16384)
	for total < 3145728 {
		chunk := int32(16384)
		if total+chunk >= 3145728 {
			chunk = 3145728 - total
		}
		result := f.add(int(chunk))
		total += chunk
		if total < 3145728 && result != 0 {
			t.Fatalf("f.add at unsent=%d returned %d, expected 0 (under threshold)", total, result)
		}
		if total >= 3145728 && result != 0 {
			// Should have sent all accumulated unsent
			if result != total {
				t.Fatalf("f.add at threshold: got %d, want %d", result, total)
			}
			return // success
		}
	}
	t.Fatal("never triggered WINDOW_UPDATE at threshold")
}

func TestInflowMinRefreshFloor(t *testing.T) {
	// For tiny windows, minRefresh should floor at inflowMinRefresh
	var f inflow
	f.init(100) // minRefresh = max(50, 4096) = 4096
	// But unsent(1) < avail(100) → buffer even though minRefresh is 4096
	if got := f.add(1); got != 0 {
		t.Fatalf("f.add(1) with window=100 = %v, want 0", got)
	}
	// When unsent >= avail, send regardless of minRefresh
	if got := f.add(99); got != 100 {
		t.Fatalf("f.add(99) with window=100 = %v, want 100", got)
	}
}

func TestTakeInflows(t *testing.T) {
	var a, b inflow
	a.init(10)
	b.init(20)
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
