// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"sync"
	"sync/atomic"
)

// requestWorkers runs doRequest invocations on a set of reusable goroutines,
// one pool per ClientConn.
//
// It exists to keep goroutine stacks grown across requests. A fresh goroutine
// per request starts on the runtime's small initial stack and re-grows it
// through header encoding and the write path every time, then dies and throws
// the grown stack away; under a steady request load that stack copying
// (runtime.newstack → copystack) plus the spawn itself (runtime.newproc) is a
// measurable slice of process CPU. A reused worker pays the growth once and
// keeps the stack hot. The GC's stack shrinking reclaims idle workers'
// stacks, so the saving is per GC cycle rather than absolute — but at many
// requests between cycles that is nearly all of it.
//
// The dispatch semantics match go-per-item exactly. dispatch never blocks and
// never queues: the handoff channel is unbuffered, so a send succeeds only
// when an idle worker is parked on a receive, and otherwise dispatch starts a
// new persistent worker (up to max) or falls back to a plain transient
// goroutine. Nothing is ever closed and no work is ever buffered, so conn
// teardown cannot strand an accepted item: an item is either already running
// on some goroutine or was never accepted.
//
// The population self-sizes to the observed concurrency high-water mark, so
// max is a bound, not a target. Workers live until done is closed — for a
// ClientConn that is readerDone, closed by the read loop's cleanup — so no
// pool goroutine outlives the connection's own read loop. Dispatching in the
// same instant done closes is safe: the accepted item still runs, on a
// worker's final lap, a fresh worker's first item, or a transient goroutine,
// exactly as a spawned item would have. Dispatches after teardown degenerate
// to one short-lived worker per item, which is the spawn behaviour they
// replace.
//
// The zero value is ready to use; dispatch is safe for concurrent use.
type requestWorkers struct {
	initOnce sync.Once
	work     chan func()
	workers  atomic.Int64
}

// dispatch runs one invocation of fn without blocking: on an idle worker if
// one is parked, on a newly started worker while the population is below max,
// and otherwise — past the cap, or in the instant a finishing worker is
// between items — on a transient goroutine, which is the go-per-item
// behaviour.
//
// done is the pool's teardown signal and max its population cap; callers pass
// the same values on every call for a given pool.
func (w *requestWorkers) dispatch(done <-chan struct{}, max int64, fn func()) {
	w.initOnce.Do(func() { w.work = make(chan func()) })
	select {
	case w.work <- fn:
		return
	default:
	}
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

// worker runs its first item immediately — dispatch starts a worker only when
// it holds an item no parked worker took — then parks on the handoff channel
// for reuse until done closes. The deferred decrement frees the worker's cap
// slot however it exits, so an item that kills its goroutine (a
// runtime.Goexit from an httptrace hook, say) costs that item's worker, not a
// pool slot forever.
func (w *requestWorkers) worker(done <-chan struct{}, fn func()) {
	defer w.workers.Add(-1)
	fn()
	for {
		select {
		case <-done:
			return
		case fn := <-w.work:
			fn()
		}
	}
}
