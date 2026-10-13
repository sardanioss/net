// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The http2 package's test files do not compile in this fork, so run this
// file's tests and benchmarks with an explicit file list, which builds only
// the pool and its tests:
//
//	go test -race -run RequestWorkers http2/request_workers.go http2/request_workers_test.go
//	go test -run - -bench . -count 10 http2/request_workers.go http2/request_workers_test.go

package http2

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequestWorkersRunsEveryDispatch(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	var wg sync.WaitGroup
	var runs atomic.Int64
	var w requestWorkers
	const n = 200
	for range n {
		wg.Add(1)
		w.dispatch(done, 4, func() {
			runs.Add(1)
			wg.Done()
		})
	}
	wg.Wait()
	if got := runs.Load(); got != n {
		t.Errorf("dispatch × %d ran the function %d times, want %d", n, got, n)
	}
	if got := w.workers.Load(); got > 4 {
		t.Errorf("pool started %d workers, cap is 4", got)
	}
}

func TestRequestWorkersReusesWorker(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	var wg sync.WaitGroup
	var w requestWorkers

	// First dispatch starts the one worker.
	wg.Add(1)
	w.dispatch(done, 8, wg.Done)
	wg.Wait()

	// A blocking send on the handoff channel completes only when a parked
	// worker receives it, so each of these items provably ran on the worker
	// started above, not on a fresh goroutine.
	for range 50 {
		wg.Add(1)
		w.work <- wg.Done
		wg.Wait()
	}
	if got := w.workers.Load(); got != 1 {
		t.Errorf("sequential reuse started %d workers, want 1", got)
	}
}

func TestRequestWorkersOverflowBeyondCap(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	const maxWorkers, extra = 4, 5
	gate := make(chan struct{})
	var started atomic.Int64
	var wg sync.WaitGroup
	var w requestWorkers

	// Every item blocks on the gate, so each dispatch finds no idle worker;
	// the first maxWorkers dispatches start workers and the rest must fall
	// back to transient goroutines. dispatch returning at all pins that it
	// does not block when the pool is saturated.
	for range maxWorkers + extra {
		wg.Add(1)
		w.dispatch(done, maxWorkers, func() {
			started.Add(1)
			<-gate
			wg.Done()
		})
	}
	waitFor(t, func() bool { return started.Load() == maxWorkers+extra })
	if got := w.workers.Load(); got != maxWorkers {
		t.Errorf("pool started %d workers, want the cap %d", got, maxWorkers)
	}
	close(gate)
	wg.Wait()
}

func TestRequestWorkersConcurrentDispatch(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	const dispatchers, perDispatcher, maxWorkers = 16, 200, 8
	var runs atomic.Int64
	var items sync.WaitGroup
	var w requestWorkers

	// Dispatch from many goroutines at once, so dispatch races itself and the
	// lazy channel init. Every item must still run exactly once and the
	// persistent population must stay within the cap; the -race build checks
	// the counter and init accesses.
	var callers sync.WaitGroup
	for range dispatchers {
		callers.Add(1)
		go func() {
			defer callers.Done()
			for range perDispatcher {
				items.Add(1)
				w.dispatch(done, maxWorkers, func() {
					runs.Add(1)
					items.Done()
				})
			}
		}()
	}
	callers.Wait()
	items.Wait()
	if got := runs.Load(); got != dispatchers*perDispatcher {
		t.Errorf("concurrent dispatch ran %d items, want %d", got, dispatchers*perDispatcher)
	}
	if got := w.workers.Load(); got > maxWorkers {
		t.Errorf("concurrent dispatch started %d workers, cap is %d", got, maxWorkers)
	}
}

func TestRequestWorkersExitOnClose(t *testing.T) {
	before := runtime.NumGoroutine()
	done := make(chan struct{})
	var wg sync.WaitGroup
	var w requestWorkers
	for range 8 {
		wg.Add(1)
		w.dispatch(done, 8, wg.Done)
	}
	wg.Wait()
	close(done)
	waitFor(t, func() bool { return runtime.NumGoroutine() <= before })
	if got := w.workers.Load(); got != 0 {
		t.Errorf("worker count is %d after teardown, want 0", got)
	}
}

func TestRequestWorkersDispatchAfterCloseStillRuns(t *testing.T) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	var w requestWorkers

	// Grow the population, then close done and let the workers drain.
	for range 2 {
		wg.Add(1)
		w.dispatch(done, 2, wg.Done)
	}
	wg.Wait()
	close(done)

	// A dispatch racing conn teardown must still run its item — on a worker's
	// final lap, a fresh worker's first item, or a transient goroutine.
	for range 8 {
		wg.Add(1)
		w.dispatch(done, 2, wg.Done)
	}
	wg.Wait()
}

func TestRequestWorkersDispatchRacesClose(t *testing.T) {
	// Unlike DispatchAfterCloseStillRuns, done closes while dispatches are
	// mid-flight, so the close races the handoff send, the CAS loop, and
	// workers' final laps. Every accepted item must still run: dispatch
	// returning means the item is on some goroutine, so once all dispatchers
	// finish, the item WaitGroup must drain.
	const dispatchers, perDispatcher = 8, 200
	done := make(chan struct{})
	var items sync.WaitGroup
	var w requestWorkers

	var callers sync.WaitGroup
	for range dispatchers {
		callers.Add(1)
		go func() {
			defer callers.Done()
			for range perDispatcher {
				items.Add(1)
				w.dispatch(done, 4, items.Done)
			}
		}()
	}
	time.Sleep(time.Millisecond)
	close(done)
	callers.Wait()

	drained := make(chan struct{})
	go func() {
		items.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("an accepted item never ran after dispatch raced close")
	}
}

func TestRequestWorkersGoexitFreesCapSlot(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	var wg sync.WaitGroup
	var w requestWorkers

	// The first dispatch runs its item on a fresh worker, and the item kills
	// that worker the way a runtime.Goexit from an httptrace hook would. The
	// deferred decrement must free the cap slot so the pool can regrow.
	wg.Add(1)
	w.dispatch(done, 1, func() {
		wg.Done()
		runtime.Goexit()
	})
	wg.Wait()
	waitFor(t, func() bool { return w.workers.Load() == 0 })

	for range 8 {
		wg.Add(1)
		w.dispatch(done, 1, wg.Done)
	}
	wg.Wait()
	if got := w.workers.Load(); got != 1 {
		t.Errorf("worker count is %d after regrowth, want 1", got)
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not reached within 5s")
}

// benchDeepStack forces stack growth comparable to doRequest's header-encode
// and write chain: ~96 frames of ~300 B grows a fresh goroutine's stack
// through several doublings.
//
//go:noinline
func benchDeepStack(n int) int {
	var buf [256]byte
	buf[0] = byte(n)
	if n == 0 {
		return int(buf[0])
	}
	return benchDeepStack(n-1) + int(buf[0])
}

var benchSink atomic.Int64

func BenchmarkRequestDispatch(b *testing.B) {
	const depth = 96

	b.Run("spawned", func(b *testing.B) {
		var wg sync.WaitGroup
		b.ReportAllocs()
		for b.Loop() {
			wg.Add(1)
			go func() {
				benchSink.Add(int64(benchDeepStack(depth)))
				wg.Done()
			}()
			wg.Wait()
		}
	})

	b.Run("pooled", func(b *testing.B) {
		done := make(chan struct{})
		defer close(done)
		var wg sync.WaitGroup
		var w requestWorkers
		fn := func() {
			benchSink.Add(int64(benchDeepStack(depth)))
			wg.Done()
		}
		b.ReportAllocs()
		for b.Loop() {
			wg.Add(1)
			w.dispatch(done, 8, fn)
			wg.Wait()
		}
	})
}

// BenchmarkStackRegrowthAcrossGC pins the claim in requestWorkers' doc
// comment that the saving survives the GC's stack shrinking. Every iteration
// forces a GC with the whole pool parked — the worst case, since a parked
// worker's near-empty live stack makes it shrinkable — then runs several
// waves of deep items. Spawned goroutines pay stack growth once per item; a
// pooled worker pays it at most once per GC cycle, so the saving is the
// amortization across the waves between cycles (a handful here, hundreds in
// production). The pool is sized to the wave, as the per-conn cap sizes it to
// the concurrency ceiling, so no item overflows to a transient goroutine.
func BenchmarkStackRegrowthAcrossGC(b *testing.B) {
	const depth = 96
	const wave = 256
	const wavesPerGC = 8

	run := func(b *testing.B, dispatch func(fn func())) {
		var wg sync.WaitGroup
		b.ReportAllocs()
		for b.Loop() {
			runtime.GC()
			for range wavesPerGC {
				wg.Add(wave)
				for range wave {
					dispatch(func() {
						benchSink.Add(int64(benchDeepStack(depth)))
						wg.Done()
					})
				}
				wg.Wait()
			}
		}
	}

	b.Run("spawned", func(b *testing.B) {
		run(b, func(fn func()) { go fn() })
	})

	b.Run("pooled", func(b *testing.B) {
		done := make(chan struct{})
		defer close(done)
		var w requestWorkers
		run(b, func(fn func()) { w.dispatch(done, wave, fn) })
	})
}
