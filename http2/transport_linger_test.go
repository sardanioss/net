// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.25 || goexperiment.synctest

// The http2 package's test files do not compile in this fork, so run this
// file's tests from the module root with an explicit file list, which builds
// the package and the synctest client harness only:
//
//	go test -run EndStreamLinger $(go list -f '{{range .GoFiles}}http2/{{.}} {{end}}' ./http2) http2/clientconn_test.go http2/connframes_test.go http2/netconn_test.go http2/synctest_go125_test.go http2/transport_linger_test.go

package http2

import (
	"io"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	http "github.com/sardanioss/http"
)

// lingerBody is the response body in these tests. The response declares its
// length, so reading it in full is what lets Close linger rather than reset.
const lingerBody = "hello"

// startLingerTest starts a GET and answers it with headers declaring a
// Content-Length of len(lingerBody), without the body yet.
func startLingerTest(t testing.TB) (*testClientConn, *testRoundTrip, *http.Response, *clientStream) {
	t.Helper()
	// Without compression the body is the transportResponseBody itself, which
	// gives the test its clientStream.
	tc := newTestClientConn(t, func(tr *Transport) { tr.DisableCompression = true })
	tc.greet()

	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt := tc.roundTrip(req)
	tc.wantFrameType(FrameHeaders)
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   rt.streamID(),
		EndHeaders: true,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "200",
			"content-length", strconv.Itoa(len(lingerBody)),
		),
	})
	res := rt.response()
	body, ok := res.Body.(transportResponseBody)
	if !ok {
		t.Fatalf("Response.Body is %T, want transportResponseBody", res.Body)
	}
	return tc, rt, res, body.cs
}

// stopLingerTimer stops the stream's linger timer, if it was armed, and
// reports whether it was still pending. The fake clock never reaches
// endStreamLinger in the tests that call it, so a pending timer is one that
// nothing stopped, and its closure keeps the stream reachable until it fires.
func stopLingerTimer(tc *testClientConn, cs *clientStream) bool {
	tc.cc.mu.Lock()
	timer := cs.lingerTimer
	tc.cc.mu.Unlock()
	return timer != nil && timer.Stop()
}

// hasStream reports whether the connection still tracks the stream, which it
// stops doing once cleanupWriteRequest has run.
func hasStream(tc *testClientConn, id uint32) bool {
	tc.cc.mu.Lock()
	defer tc.cc.mu.Unlock()
	_, ok := tc.cc.streams[id]
	return ok
}

// When the last DATA frame carries END_STREAM, the stream is cleaned up before
// the caller gets round to closing the body. Close must not arm the linger
// timer then, since cleanup, the only thing that stops it, has already run.
func TestTransportEndStreamLingerNotArmedAfterEndStream(t *testing.T) {
	synctestTest(t, testTransportEndStreamLingerNotArmedAfterEndStream)
}
func testTransportEndStreamLingerNotArmedAfterEndStream(t testing.TB) {
	tc, rt, res, cs := startLingerTest(t)
	tc.writeData(rt.streamID(), true, []byte(lingerBody))
	got, err := io.ReadAll(res.Body)
	if err != nil || string(got) != lingerBody {
		t.Fatalf("ReadAll(Response.Body) = %q, %v; want %q, nil", got, err, lingerBody)
	}
	synctest.Wait()
	if hasStream(tc, rt.streamID()) {
		t.Fatalf("stream still open after END_STREAM; want it cleaned up before Close")
	}

	if err := res.Body.Close(); err != nil {
		t.Errorf("Response.Body.Close() = %v, want nil", err)
	}
	synctest.Wait()
	if stopLingerTimer(tc, cs) {
		t.Errorf("linger timer pending after Close of an ended stream; want none armed")
	}
	tc.wantIdle()
}

// A request cancelled after its body was read in full, but before the peer's
// END_STREAM, is reset by cleanup. Close afterwards must not arm the linger
// timer for a stream that is already over.
func TestTransportEndStreamLingerNotArmedAfterCancel(t *testing.T) {
	synctestTest(t, testTransportEndStreamLingerNotArmedAfterCancel)
}
func testTransportEndStreamLingerNotArmedAfterCancel(t testing.TB) {
	tc, rt, res, cs := startLingerTest(t)
	tc.writeData(rt.streamID(), false, []byte(lingerBody))
	got := make([]byte, len(lingerBody))
	if _, err := io.ReadFull(res.Body, got); err != nil {
		t.Fatalf("ReadFull(Response.Body) = %v, want nil", err)
	}

	rt.cancel()
	synctest.Wait()
	tc.wantRSTStream(rt.streamID(), ErrCodeCancel)

	if err := res.Body.Close(); err != nil {
		t.Errorf("Response.Body.Close() = %v, want nil", err)
	}
	synctest.Wait()
	if stopLingerTimer(tc, cs) {
		t.Errorf("linger timer pending after Close of a cancelled stream; want none armed")
	}
	tc.wantIdle()
}

// Closing a fully read body before the peer's END_STREAM, which servers
// commonly send a moment later in an empty DATA frame, lingers for that frame
// rather than resetting the stream (#117). Its arrival ends the stream
// cleanly and stops the timer.
func TestTransportEndStreamLingerEndsOnEmptyEndStream(t *testing.T) {
	synctestTest(t, testTransportEndStreamLingerEndsOnEmptyEndStream)
}
func testTransportEndStreamLingerEndsOnEmptyEndStream(t testing.TB) {
	tc, rt, res, cs := startLingerTest(t)
	tc.writeData(rt.streamID(), false, []byte(lingerBody))
	got := make([]byte, len(lingerBody))
	if _, err := io.ReadFull(res.Body, got); err != nil {
		t.Fatalf("ReadFull(Response.Body) = %v, want nil", err)
	}
	if err := res.Body.Close(); err != nil {
		t.Errorf("Response.Body.Close() = %v, want nil", err)
	}
	synctest.Wait()
	if !lingering(cs) {
		t.Errorf("stream not lingering after Close of a fully read body; want it waiting for END_STREAM")
	}
	tc.wantIdle()

	tc.writeData(rt.streamID(), true, nil)
	synctest.Wait()
	if stopLingerTimer(tc, cs) {
		t.Errorf("linger timer pending after END_STREAM; want it stopped by cleanup")
	}
	if hasStream(tc, rt.streamID()) {
		t.Errorf("stream still open after END_STREAM; want it cleaned up")
	}
	tc.wantIdle()
}

// A peer that never sends END_STREAM after a fully read, closed body has the
// stream reset once endStreamLinger has passed, so it cannot hold a stream
// slot for the life of the connection.
func TestTransportEndStreamLingerTimeoutResets(t *testing.T) {
	synctestTest(t, testTransportEndStreamLingerTimeoutResets)
}
func testTransportEndStreamLingerTimeoutResets(t testing.TB) {
	tc, rt, res, _ := startLingerTest(t)
	tc.writeData(rt.streamID(), false, []byte(lingerBody))
	got := make([]byte, len(lingerBody))
	if _, err := io.ReadFull(res.Body, got); err != nil {
		t.Fatalf("ReadFull(Response.Body) = %v, want nil", err)
	}
	if err := res.Body.Close(); err != nil {
		t.Errorf("Response.Body.Close() = %v, want nil", err)
	}
	synctest.Wait()
	tc.wantIdle()

	time.Sleep(endStreamLinger + time.Second)
	synctest.Wait()
	tc.wantRSTStream(rt.streamID(), ErrCodeCancel)
	if hasStream(tc, rt.streamID()) {
		t.Errorf("stream still open after the linger timer fired; want it cleaned up")
	}
	tc.wantIdle()
}
