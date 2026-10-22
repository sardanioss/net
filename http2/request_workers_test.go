// Copyright 2026 The Go Authors. All rights reserved.
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
	"math/rand/v2"
	"runtime"
	"slices"
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
	waitGroup(t, &wg)
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
	waitGroup(t, &wg)
	waitFor(t, func() bool { return len(parkedWorkers(&w)) == 1 })
	first := parkedWorkers(&w)[0]

	// With that worker parked, each dispatch pops it, so each item provably
	// runs on the worker started above, not on a fresh goroutine.
	for range 50 {
		wg.Add(1)
		w.dispatch(done, 8, wg.Done)
		waitGroup(t, &wg)
		waitFor(t, func() bool {
			p := parkedWorkers(&w)
			return len(p) == 1 && p[0] == first
		})
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
	waitGroup(t, &wg)
}

func TestRequestWorkersConcurrentDispatch(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	const dispatchers, perDispatcher, maxWorkers = 16, 200, 8
	var runs atomic.Int64
	var items sync.WaitGroup
	var w requestWorkers

	// Dispatch from many goroutines at once, so dispatch races itself and the
	// workers parking. Every item must still run exactly once and the
	// persistent population must stay within the cap; the -race build checks
	// the stack and counter accesses.
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
	waitGroup(t, &callers, &items)
	if got := runs.Load(); got != dispatchers*perDispatcher {
		t.Errorf("concurrent dispatch ran %d items, want %d", got, dispatchers*perDispatcher)
	}
	if got := w.workers.Load(); got > maxWorkers {
		t.Errorf("concurrent dispatch started %d workers, cap is %d", got, maxWorkers)
	}
}

func TestRequestWorkersExitOnClose(t *testing.T) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	var w requestWorkers
	for range 8 {
		wg.Add(1)
		w.dispatch(done, 8, wg.Done)
	}
	waitGroup(t, &wg)
	close(done)
	waitFor(t, func() bool { return w.workers.Load() == 0 })
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
	waitGroup(t, &wg)
	close(done)

	// A dispatch racing conn teardown must still run its item — on a worker's
	// final lap, a fresh worker's first item, or a transient goroutine.
	for range 8 {
		wg.Add(1)
		w.dispatch(done, 2, wg.Done)
	}
	waitGroup(t, &wg)
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

	var dispatched atomic.Int64
	var callers sync.WaitGroup
	for range dispatchers {
		callers.Add(1)
		go func() {
			defer callers.Done()
			for range perDispatcher {
				items.Add(1)
				w.dispatch(done, 4, items.Done)
				dispatched.Add(1)
			}
		}()
	}
	waitFor(t, func() bool { return dispatched.Load() >= dispatchers*perDispatcher/4 })
	close(done)
	waitGroup(t, &callers, &items)
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
	waitGroup(t, &wg)
	waitFor(t, func() bool { return w.workers.Load() == 0 })

	for range 8 {
		wg.Add(1)
		w.dispatch(done, 1, wg.Done)
	}
	waitGroup(t, &wg)
	if got := w.workers.Load(); got != 1 {
		t.Errorf("worker count is %d after regrowth, want 1", got)
	}
}

// parkInOrder starts n workers, each held on its own gate, then releases them
// one at a time so they park in a known order, and returns the stack.
func parkInOrder(t *testing.T, w *requestWorkers, done <-chan struct{}, n int) []*requestWorker {
	t.Helper()
	gates := make([]chan struct{}, n)
	for i := range gates {
		gates[i] = make(chan struct{})
		gate := gates[i]
		w.dispatch(done, int64(n), func() { <-gate })
	}
	for i, gate := range gates {
		close(gate)
		waitFor(t, func() bool { return len(parkedWorkers(w)) == i+1 })
	}
	return parkedWorkers(w)
}

func TestRequestWorkersReusesMostRecentlyParked(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	w := requestWorkers{idleTimeout: time.Hour}
	parked := parkInOrder(t, &w, done, 3)

	// Each dispatch takes the worker that parked last; the ones below it
	// stay where they are.
	for want := len(parked); want > 0; want-- {
		gate := make(chan struct{})
		defer close(gate)
		w.dispatch(done, 3, func() { <-gate })
		if got := parkedWorkers(&w); !slices.Equal(got, parked[:want-1]) {
			t.Fatalf("after a dispatch the parked stack is %p, want %p", got, parked[:want-1])
		}
		if got := staleSlots(&w); got != 0 {
			t.Errorf("after a dispatch %d vacated stack slots still hold a worker", got)
		}
	}
}

func TestRequestWorkersSurplusExpiresUnderLoad(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	const surplus = 8
	w := requestWorkers{idleTimeout: 50 * time.Millisecond}
	gate := make(chan struct{})
	for range surplus {
		w.dispatch(done, surplus, func() { <-gate })
	}
	close(gate)

	// One caller keeps the pool busy, one item at a time, for longer than the
	// idle timeout. The worker on top of the stack serves it every time, so
	// the workers below it expire while the load goes on.
	stop := make(chan struct{})
	loadDone := make(chan struct{})
	go func() {
		defer close(loadDone)
		var wg sync.WaitGroup
		for {
			select {
			case <-stop:
				return
			default:
			}
			wg.Add(1)
			w.dispatch(done, surplus, wg.Done)
			wg.Wait()
		}
	}()
	waitFor(t, func() bool { return w.workers.Load() <= 2 })
	if got := staleSlots(&w); got != 0 {
		t.Errorf("after expiry %d vacated stack slots still hold a worker", got)
	}
	close(stop)
	<-loadDone
	if got := w.workers.Load(); got == 0 {
		t.Error("every worker expired, including the one serving the load")
	}
}

func TestRequestWorkersReuseKeepsWorker(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	const timeout = 200 * time.Millisecond
	w := requestWorkers{idleTimeout: timeout}
	worker := parkInOrder(t, &w, done, 1)[0]

	// Use the worker every timeout/4 for several timeouts in all. Each use
	// parks it afresh, so it is never idle long enough to expire.
	var wg sync.WaitGroup
	for deadline := time.Now().Add(4 * timeout); time.Now().Before(deadline); {
		wg.Add(1)
		w.dispatch(done, 1, wg.Done)
		waitGroup(t, &wg)
		waitFor(t, func() bool { return len(parkedWorkers(&w)) == 1 })
		if got := parkedWorkers(&w)[0]; got != worker {
			t.Fatal("a reused worker was replaced; it expired while in use")
		}
		time.Sleep(timeout / 4) // the gap between uses, not synchronisation
	}
}

func TestRequestWorkersExpiryFreesCapSlot(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	w := requestWorkers{idleTimeout: time.Millisecond}
	var wg sync.WaitGroup
	wg.Add(1)
	w.dispatch(done, 1, wg.Done)
	waitGroup(t, &wg)
	waitFor(t, func() bool { return w.workers.Load() == 0 && len(parkedWorkers(&w)) == 0 })
	w.mu.Lock()
	kept := cap(w.parked)
	w.mu.Unlock()
	if kept != 0 {
		t.Errorf("the emptied stack kept a backing array of %d slots", kept)
	}

	// The cap is one and the only worker has expired, so the next dispatch
	// must be able to start a worker again rather than overflow.
	gate := make(chan struct{})
	defer close(gate)
	w.dispatch(done, 1, func() { <-gate })
	if got := w.workers.Load(); got != 1 {
		t.Errorf("worker count after expiry and a new dispatch is %d, want 1", got)
	}
}

func TestRequestWorkersDispatchRacesExpiry(t *testing.T) {
	// The expiry timer fires almost as soon as a worker parks, so dispatch
	// and expiry keep competing for the same parked workers. Every item must
	// run exactly once.
	const dispatchers, perDispatcher = 8, 500
	done := make(chan struct{})
	defer close(done)
	w := requestWorkers{idleTimeout: time.Nanosecond}
	var runs [dispatchers * perDispatcher]atomic.Int32
	var items, callers sync.WaitGroup
	for d := range dispatchers {
		callers.Add(1)
		go func() {
			defer callers.Done()
			for i := range perDispatcher {
				items.Add(1)
				run := &runs[d*perDispatcher+i]
				w.dispatch(done, 4, func() {
					run.Add(1)
					items.Done()
				})
			}
		}()
	}
	waitGroup(t, &callers, &items)
	for i := range runs {
		if got := runs[i].Load(); got != 1 {
			t.Fatalf("item %d ran %d times, want 1", i, got)
		}
	}
}

func TestRequestWorkersTeardownWithPendingExpiry(t *testing.T) {
	done := make(chan struct{})
	w := requestWorkers{idleTimeout: time.Hour}
	parkInOrder(t, &w, done, 8)
	close(done)
	waitFor(t, func() bool { return w.workers.Load() == 0 })
	if got := len(parkedWorkers(&w)); got != 0 {
		t.Errorf("%d workers still parked after teardown, want 0", got)
	}
	if got := staleSlots(&w); got != 0 {
		t.Errorf("after teardown %d vacated stack slots still hold a worker", got)
	}
	if expiryArmed(&w) {
		t.Error("the expiry timer is still pending after teardown")
	}
}

func TestRequestWorkersTeardownRacesExpiry(t *testing.T) {
	// The expiry timer fires continually while the workers park and done
	// closes, so teardown races expiry for every worker. Nothing may be
	// left: no worker, no parked entry, no pending timer.
	for range 50 {
		done := make(chan struct{})
		w := requestWorkers{idleTimeout: time.Nanosecond}
		gate := make(chan struct{})
		for range 8 {
			w.dispatch(done, 8, func() { <-gate })
		}
		close(gate)
		close(done)
		waitFor(t, func() bool {
			return w.workers.Load() == 0 && len(parkedWorkers(&w)) == 0 && !expiryArmed(&w)
		})
	}
}

// staleSlots counts the slots past the end of w's parked stack, within its
// capacity, that still point at a worker.
func staleSlots(w *requestWorkers) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, rw := range w.parked[len(w.parked):cap(w.parked)] {
		if rw != nil {
			n++
		}
	}
	return n
}

// expiryArmed reports whether w's expiry timer is pending or still running.
func expiryArmed(w *requestWorkers) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.armed
}

func TestRequestWorkersReuseDoesNotAllocate(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	w := requestWorkers{idleTimeout: time.Hour}
	ran := make(chan struct{}, 1)
	fn := func() { ran <- struct{}{} }
	parked := func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.parked) == 1
	}

	// The first run starts the worker, its handoff channel and the expiry
	// timer. Every later run pops that worker, hands it fn and waits for it
	// to park again, which must not allocate.
	var timedOut bool
	allocs := testing.AllocsPerRun(100, func() {
		w.dispatch(done, 1, fn)
		<-ran
		for deadline := time.Now().Add(5 * time.Second); !parked(); {
			if time.Now().After(deadline) {
				timedOut = true
				return
			}
			runtime.Gosched()
		}
	})
	if timedOut {
		t.Fatal("the worker did not park again within 5s")
	}
	if allocs != 0 {
		t.Errorf("dispatch to a parked worker and its return to the stack allocated %v times per item, want 0", allocs)
	}
}

func TestRequestWorkersStress(t *testing.T) {
	// Each round runs a seeded mix of dispatches, items that yield, items
	// that end their goroutine with runtime.Goexit, an idle timeout short
	// enough to expire workers between items, and a teardown at a seeded
	// point among the dispatches. Whatever the interleaving, every item runs
	// exactly once and the pool ends with no worker, no parked entry, no
	// pending timer and no goroutine left behind.
	base := runtime.NumGoroutine()
	for seed := range uint64(40) {
		r := rand.New(rand.NewPCG(seed, 0))
		const dispatchers, perDispatcher = 4, 100
		timeouts := []time.Duration{time.Nanosecond, time.Microsecond, 50 * time.Microsecond}
		w := requestWorkers{idleTimeout: timeouts[r.IntN(len(timeouts))]}
		done := make(chan struct{})
		closeAt := int64(r.IntN(dispatchers * perDispatcher))
		var dispatched atomic.Int64
		var closeOnce sync.Once
		var runs [dispatchers * perDispatcher]atomic.Int32
		var items, callers sync.WaitGroup
		for d := range dispatchers {
			kinds := make([]int, perDispatcher)
			for i := range kinds {
				kinds[i] = r.IntN(8)
			}
			callers.Add(1)
			go func() {
				defer callers.Done()
				for i, kind := range kinds {
					run := &runs[d*perDispatcher+i]
					items.Add(1)
					w.dispatch(done, 4, func() {
						run.Add(1)
						items.Done()
						switch kind {
						case 0:
							runtime.Goexit()
						case 1, 2:
							runtime.Gosched()
						}
					})
					if dispatched.Add(1) == closeAt {
						closeOnce.Do(func() { close(done) })
					}
				}
			}()
		}
		waitGroup(t, &callers, &items)
		closeOnce.Do(func() { close(done) })
		for i := range runs {
			if got := runs[i].Load(); got != 1 {
				t.Fatalf("seed %d: item %d ran %d times, want 1", seed, i, got)
			}
		}
		waitFor(t, func() bool {
			return w.workers.Load() == 0 && len(parkedWorkers(&w)) == 0 && !expiryArmed(&w)
		})
	}
	// waitFor's own polling goroutine is the one goroutine allowed over base.
	waitFor(t, func() bool { return runtime.NumGoroutine() <= base+1 })
}

// parkedWorkers returns a copy of w's parked stack, most recent last.
func parkedWorkers(w *requestWorkers) []*requestWorker {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.parked)
}

// waitFor polls cond until it holds, and fails the test if it does not hold
// within 5s. cond is polled on another goroutine, so a cond that blocks, on a
// pool mutex a bug left held, say, fails the test too instead of hanging it.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	reached, stop := make(chan struct{}), make(chan struct{})
	go func() {
		for !cond() {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
		close(reached)
	}()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		close(stop)
		t.Fatal("condition not reached within 5s")
	}
}

// waitGroup waits for each of wgs in turn and fails the test if they have not
// all finished within 10s, so a lost item or a blocked dispatch fails the test
// instead of hanging it.
func waitGroup(t *testing.T, wgs ...*sync.WaitGroup) {
	t.Helper()
	finished := make(chan struct{})
	go func() {
		for _, wg := range wgs {
			wg.Wait()
		}
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatched items did not all finish within 10s")
	}
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
