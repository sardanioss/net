// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"cmp"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// workerIdleTimeout is how long a request worker stays parked with no work
// before it exits. The pool checks for idle workers once per timeout, so a
// worker exits once it has been idle for more than one timeout and at most
// two, plus timer latency.
//
// A worker is kept to save the stack it grew on earlier requests, and that
// saving fades on its own: the GC halves a parked goroutine's stack whenever
// it is using less than a quarter of it, so a couple of GC cycles return an
// idle worker to roughly the stack a new goroutine starts with. Past that,
// keeping it saves only a goroutine start, which is cheap. fasthttp's worker
// pool expires idle workers after the same 10 seconds by default
// (MaxIdleWorkerDuration), with the same once-per-timeout check.
const workerIdleTimeout = 10 * time.Second

// requestWorkers runs doRequest invocations on a set of reusable goroutines,
// one pool per ClientConn.
//
// It exists to keep goroutine stacks grown across requests. A fresh goroutine
// per request starts on the runtime's small initial stack and re-grows it
// through header encoding and the write path every time, then dies and throws
// the grown stack away; under a steady request load that stack copying
// (runtime.newstack, then copystack) plus the spawn itself (runtime.newproc)
// is a measurable slice of process CPU. A reused worker pays the growth once
// and keeps the stack hot.
//
// Parked workers are kept on a stack, and dispatch hands an item to the one
// that parked most recently. Handing work to the longest parked worker
// instead, as a shared channel does, rotates a steady load through every
// worker the pool ever started, so none of them is ever idle long enough to
// expire and the population stays at the burst's high-water mark. With the
// most recent worker first, the workers a load needs stay busy and the
// surplus sits untouched at the bottom of the stack until it expires, freeing
// its slot under max.
//
// Expiry is one timer per pool rather than one per worker. A worker's own
// timer would have to be selected on, and restarted, every time the worker
// parks, which made each handoff measurably slower. Instead each worker
// records the pool's expiry tick count when it parks, under the mutex that
// already covers the stack, and the pool's timer fires once per idle timeout
// from the first park until it finds the stack empty. Because the stack is in
// parking order, the workers that have been parked since before the previous
// tick are at its bottom; the timer takes them off and sends each a nil item,
// which tells it to exit.
//
// The dispatch semantics match go-per-item exactly. dispatch never blocks and
// never queues: it pops a parked worker if there is one and hands it the item
// through that worker's own one-slot channel, and otherwise starts a new
// worker (up to max) or falls back to a plain transient goroutine. Workers
// live until they expire or done is closed (for a ClientConn that is
// readerDone, closed by the read loop's cleanup), so no pool goroutine
// outlives the connection's own read loop. dispatch and the expiry timer take
// a worker off the stack under the mutex and then hand it exactly one value,
// an item or nil, so neither can reach a worker the other took. A worker that
// sees done closed exits only if it can still take itself off the stack; if
// it was already taken, it receives what it was handed first, so an item
// dispatched in the same instant done closes still runs.
//
// The zero value is ready to use; dispatch is safe for concurrent use.
type requestWorkers struct {
	mu     sync.Mutex
	parked []*requestWorker // in parking order, so the most recent is last
	ticks  uint64           // times expiry has fired
	expiry *time.Timer      // runs expire; created on first park
	armed  bool             // expiry is pending, or running and not yet done

	workers atomic.Int64

	// idleTimeout overrides workerIdleTimeout when set. Tests shorten it.
	idleTimeout time.Duration
}

// requestWorker is a parked worker's handoff point. Taking a worker off the
// stack entitles dispatch or the expiry timer to send it exactly one value,
// and the worker parks again only after receiving it, so the channel's single
// slot means that send never waits for the worker.
type requestWorker struct {
	work     chan func()
	parkedAt uint64 // requestWorkers.ticks when the worker last parked
}

// dispatch runs one invocation of fn without blocking: on the most recently
// parked worker if one is parked, on a newly started worker while the
// population is below max, and otherwise (past the cap, or in the instant a
// finishing worker is between items) on a transient goroutine, which is the
// go-per-item behaviour.
//
// done is the pool's teardown signal and max its population cap; callers pass
// the same values on every call for a given pool.
func (w *requestWorkers) dispatch(done <-chan struct{}, max int64, fn func()) {
	w.mu.Lock()
	if n := len(w.parked); n > 0 {
		rw := w.parked[n-1]
		w.parked[n-1] = nil
		w.parked = w.parked[:n-1]
		w.mu.Unlock()
		rw.work <- fn
		return
	}
	w.mu.Unlock()
	for {
		n := w.workers.Load()
		if n >= max {
			go fn()
			return
		}
		if w.workers.CompareAndSwap(n, n+1) {
			go w.worker(done, fn)
			return
		}
	}
}

// worker runs its first item immediately (dispatch starts a worker only when
// it holds an item no parked worker took), then parks for reuse until it is
// handed nil by the expiry timer or done closes. The deferred decrement frees
// the worker's cap slot however it exits, so an item that kills its goroutine
// (a runtime.Goexit from an httptrace hook, say) costs that item's worker,
// not a pool slot forever.
func (w *requestWorkers) worker(done <-chan struct{}, fn func()) {
	defer w.workers.Add(-1)
	rw := &requestWorker{work: make(chan func(), 1)}
	for fn != nil {
		fn()
		w.park(rw)
		select {
		case fn = <-rw.work:
		case <-done:
			if w.leave(rw) {
				return
			}
			fn = <-rw.work
		}
	}
}

// park pushes rw onto the stack and starts the expiry timer if it is not
// already pending.
func (w *requestWorkers) park(rw *requestWorker) {
	w.mu.Lock()
	defer w.mu.Unlock()
	rw.parkedAt = w.ticks
	w.parked = append(w.parked, rw)
	if w.armed {
		return
	}
	w.armed = true
	if w.expiry == nil {
		w.expiry = time.AfterFunc(w.timeout(), w.expire)
	} else {
		w.expiry.Reset(w.timeout())
	}
}

// expire runs once per idle timeout while any worker is parked. It tells the
// workers that have been parked since before its previous run, and so idle
// for at least a full timeout, to exit, and stops once the stack is empty.
func (w *requestWorkers) expire() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ticks++
	n := 0
	for n < len(w.parked) && w.parked[n].parkedAt+2 <= w.ticks {
		w.parked[n].work <- nil
		n++
	}
	w.parked = slices.Delete(w.parked, 0, n)
	if len(w.parked) == 0 {
		// Drop the backing array too, which a burst may have grown, so an
		// idle connection keeps none of it.
		w.parked = nil
		w.armed = false
		return
	}
	w.expiry.Reset(w.timeout())
}

// leave takes rw off the parked stack and reports whether it was still there.
// When it was not, dispatch or expire has already taken it and is handing it
// an item or nil, which the worker must receive before it may leave. The last
// worker to leave stops the expiry timer, so teardown leaves no timer behind.
func (w *requestWorkers) leave(rw *requestWorker) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	i := slices.Index(w.parked, rw)
	if i < 0 {
		return false
	}
	w.parked = slices.Delete(w.parked, i, i+1)
	if len(w.parked) == 0 && w.armed && w.expiry.Stop() {
		w.armed = false
	}
	return true
}

func (w *requestWorkers) timeout() time.Duration {
	return cmp.Or(w.idleTimeout, workerIdleTimeout)
}
