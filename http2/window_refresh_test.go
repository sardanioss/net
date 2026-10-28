package http2

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	http "github.com/sardanioss/http"
	"github.com/sardanioss/net/http2/hpack"
)

// WindowRefresh: Firefox's rule replayed against Firefox's own bookkeeping, and
// end to end over an in-memory connection.
//
// This file shares recordingConn, peerFrame and peerWrite with
// stream_start_test.go, and the package's other test files do not build
// against the forked net/http, so the two run together:
//
//	go test $(go list -f '{{join .GoFiles " "}}' .) stream_start_test.go window_refresh_test.go

const (
	firefoxMinimumToAck = 4194304  // Http2Session.h kMinimumToAck
	firefoxEmergency    = 98304    // Http2Session.h kEmergencyWindowThreshold
	firefoxPipe         = 786432   // nsHttpTransaction's response pipe, 24 segments of 32768
	firefoxConnFlow     = 12517377 // the session bump, mInitialRwin - 65535
	firefoxRwin         = 12582912 // network.http.http2.pull-allowance
)

func firefoxRefresh() *WindowRefresh {
	return &WindowRefresh{
		OnArrival:  true,
		AckAt:      firefoxMinimumToAck,
		Floor:      firefoxEmergency,
		ReadBuffer: firefoxPipe,
	}
}

// firefoxRefreshTransport is firefoxLikeTransport(true) with Firefox's session
// bump and refresh rule.
func firefoxRefreshTransport() *Transport {
	t := firefoxLikeTransport(true)
	t.ConnectionFlow = firefoxConnFlow
	t.WindowRefresh = firefoxRefresh()
	return t
}

// ------------------------------------------------------------ against Firefox

// Firefox 159.0 logs its window bookkeeping at nsHttp:4. For each DATA frame,
// UpdateLocalStreamWindow logs the frame's length, LocalUnAcked (the bytes it
// would hand back, after subtracting what its consumer had not taken) and the
// stream's window, and UpdateLocalSessionWindow logs the session's. Each logs
// the size of any update it then sends. When its consumer drains bytes a
// stream had to keep, ProcessSlowConsumer runs the stream's check again with
// no new bytes, and that is logged the same way.
//
// The lines come from one connection in each of three captures of Firefox
// 159.0 on Windows. Each fetched 20 MiB bodies over direct HTTPS from a server
// that sent 16384-byte frames as fast as the windows allowed:
//
//   - read: one body, read with arrayBuffer().
//   - two: two of them at once.
//   - unread: one that the page never reads.
//
// Replayed in full, the three logs, 10549 lines with 46 updates, matched line
// for line. testdata/firefox159-window-refresh.txt keeps 18 of those lines, in
// six runs that between them take every branch of the rule:
//
//   - a stream, and then the session, reach AckAt and hand back exactly that
//     (two);
//   - only what is not held counts toward AckAt, and what is held stays
//     waiting when the rest goes back (read);
//   - at Floor, a stream hands back what is not held, far under AckAt (read);
//   - with every waiting byte held, nothing goes back, even at Floor and at a
//     window of 0 (unread);
//   - a check with no new bytes hands back by Floor (unread) and by AckAt
//     (two);
//   - everywhere else nothing goes back, though an hour passes between
//     lines: Firefox has no timer.
//
// Each run is replayed into a fork inflow that starts where Firefox's stood
// just before the run's first line. In Firefox a scope's window and the bytes
// it has received and not acked always add up to the 12 MiB it opened with,
// so the window it logged says how much was waiting. The held-back share comes
// the same way: those bytes, less LocalUnAcked. The test then checks the
// window the fork has left against Firefox's, and that the fork sends exactly
// the updates Firefox sent, in size and in position. Holding back by the pipe
// is checked end to end further down, since only Firefox's own consumer
// decided how much was held here.
func TestWindowRefreshReplaysFirefox(t *testing.T) {
	f, err := os.Open("testdata/firefox159-window-refresh.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	rule := firefoxRefresh()
	now := time.Now()
	scopes := map[uint32]*inflow{} // by stream ID; the session is 0
	updates := 0

	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := sc.Text()
		if text == "" {
			clear(scopes) // a cut: each scope starts again at its next line
			continue
		}
		if strings.HasPrefix(text, "#") {
			continue
		}
		var (
			kind                       string
			id                         uint32
			n, logged, window, wantAck int
		)
		if _, err := fmt.Sscan(text, &kind, &id, &n, &logged, &window, &wantAck); err != nil {
			t.Fatalf("line %d: %v", line, err)
		}
		if (kind != "C" && kind != "S") || (kind == "C") != (id == 0) {
			t.Fatalf("line %d: %s on stream %d", line, kind, id)
		}
		// An hour between events: Firefox has no timer, so neither may the
		// rule.
		now = now.Add(time.Hour)
		fl := scopes[id]
		if fl == nil {
			// The run's first line: start where Firefox stood before it,
			// with the rest of the 12 MiB waiting.
			fl = &inflow{rule: rule}
			fl.init(firefoxRwin, now)
			fl.avail = int32(window + n)
			fl.unsent = firefoxRwin - fl.avail
			scopes[id] = fl
		}
		held := 0
		if kind == "S" {
			// Received and not acked, less LocalUnAcked.
			held = firefoxRwin - window - logged
		}
		if !fl.take(uint32(n)) {
			t.Fatalf("line %d: %d bytes overran the window", line, n)
		}
		if int(fl.avail) != window {
			t.Fatalf("line %d: window %d, Firefox had %d", line, fl.avail, window)
		}
		got := fl.addHeld(n, held, now)
		if int(got) != wantAck {
			t.Fatalf("line %d (%s): update %d, Firefox sent %d", line, text, got, wantAck)
		}
		if got != 0 {
			updates++
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if updates == 0 {
		t.Fatal("no updates replayed; the testdata is empty")
	}
}

// The zero AckAt is Chromium's "more than half the window", and with
// Chromium's Interval a rule decides exactly as the default does, on the same
// inputs.
func TestWindowRefreshZeroAckAtIsTheDefault(t *testing.T) {
	steps := []struct {
		n     int
		after time.Duration
	}{
		{1000, 0}, {31000, time.Second}, {1, time.Second}, // past half of 64000
		{10, time.Second}, {10, 5 * time.Second}, // the timer
		{0, 10 * time.Second}, {32000, 0}, {1, 0},
	}
	now := time.Now()
	var def, rule inflow
	rule.rule = &WindowRefresh{Interval: inflowSmallUpdateInterval}
	def.init(64000, now)
	rule.init(64000, now)
	for i, s := range steps {
		now = now.Add(s.after)
		def.take(uint32(s.n))
		rule.take(uint32(s.n))
		if a, b := def.add(s.n, now), rule.add(s.n, now); a != b {
			t.Fatalf("step %d: the default sent %d, the rule %d", i, a, b)
		}
	}
}

func TestWindowRefreshInterval(t *testing.T) {
	now := time.Now()
	var f inflow
	f.rule = &WindowRefresh{AckAt: 1 << 30, Interval: time.Second}
	f.init(1<<20, now)
	if got := f.add(10, now.Add(999*time.Millisecond)); got != 0 {
		t.Fatalf("sent %d before the interval", got)
	}
	if got := f.add(10, now.Add(time.Second)); got != 20 {
		t.Fatalf("sent %d at the interval, want 20", got)
	}
	if got := f.add(10, now.Add(1500*time.Millisecond)); got != 0 {
		t.Fatalf("sent %d half an interval after the last update", got)
	}
}

// --------------------------------------------------------------- end to end

// A reader that keeps up never leaves more than the pipe unread, so Firefox's
// rule depends on arrivals alone. Every 4 MiB, the stream and the connection
// each hand back exactly 4194304, the stream's first, in one write. The frame
// that ends the stream counts for the connection only.
func TestFirefoxRefreshPromptReader(t *testing.T) {
	const body = 20 << 20 // 1280 frames; the last carries END_STREAM
	p := newWindowPeer(t, body)
	reader := p.lockstep(32 << 14) // the peer waits for the reader every 32 frames
	cc, err := firefoxRefreshTransport().NewClientConn(p.conn)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	res := p.get(t, cc)
	if n, err := reader.drain(res.Body); err != nil || n != body {
		t.Fatalf("read %d bytes (err %v), want %d", n, err, body)
	}
	res.Body.Close()
	p.waitResponses()

	want := [][]peerFrame{
		{{FrameWindowUpdate, 3, 4194304}, {FrameWindowUpdate, 0, 4194304}},
		{{FrameWindowUpdate, 3, 4194304}, {FrameWindowUpdate, 0, 4194304}},
		{{FrameWindowUpdate, 3, 4194304}, {FrameWindowUpdate, 0, 4194304}},
		{{FrameWindowUpdate, 3, 4194304}, {FrameWindowUpdate, 0, 4194304}},
		{{FrameWindowUpdate, 0, 4194304}},
	}
	if got := refreshWrites(p.conn); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("refresh writes = %v, want %v", got, want)
	}
}

// A reader that reads nothing holds the stream back beyond the pipe. The
// connection still hands back every 4 MiB that arrives. The stream sends
// nothing until its window falls to the floor, then hands back the pipe and
// the frame that arrived with it, 802816. That is the plateau Firefox's log
// shows when its consumer stalls. The peer then runs the stream window dry.
// A reader that drains it afterwards must get the transfer going again, which
// only the check that runs on reads can do.
func TestFirefoxRefreshStalledReader(t *testing.T) {
	const (
		body   = 20 << 20
		frames = (firefoxRwin + firefoxPipe + 1<<14) / (1 << 14) // what the stream window allows
	)
	p := newWindowPeer(t, body)
	cc, err := firefoxRefreshTransport().NewClientConn(p.conn)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	res := p.get(t, cc)
	p.waitSent(t, 3, frames<<14)
	time.Sleep(100 * time.Millisecond) // nothing more may follow
	want := [][]peerFrame{
		{{FrameWindowUpdate, 0, 4194304}}, // 256 frames
		{{FrameWindowUpdate, 0, 4194304}}, // 512
		{{FrameWindowUpdate, 3, 802816}},  // 762: the stream window is at the floor
		{{FrameWindowUpdate, 0, 4194304}}, // 768
	}
	if got := refreshWrites(p.conn); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("with the reader stalled, refresh writes = %v, want %v", got, want)
	}
	if sent := p.sentOn(3); sent != frames<<14 {
		t.Fatalf("peer sent %d bytes, want the %d the stream window allows", sent, frames<<14)
	}

	n, err := io.Copy(io.Discard, res.Body)
	if err != nil || n != body {
		t.Fatalf("after the stall, read %d bytes (err %v), want %d", n, err, body)
	}
	res.Body.Close()
}

// Under the default rule, the same stalled reader returns nothing at all,
// because credit only counts once read. That is the difference the arrival
// rule makes at the connection.
func TestDefaultRefreshStalledReaderReturnsNothing(t *testing.T) {
	p := newWindowPeer(t, 20<<20)
	tr := firefoxLikeTransport(true)
	tr.ConnectionFlow = firefoxConnFlow
	cc, err := tr.NewClientConn(p.conn)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	res := p.get(t, cc)
	p.waitSent(t, 3, firefoxRwin)
	time.Sleep(100 * time.Millisecond)
	if got := refreshWrites(p.conn); len(got) != 0 {
		t.Fatalf("refresh writes = %v, want none before anything is read", got)
	}
	res.Body.Close()
}

// Under an arrival rule the connection has counted every byte by the time the
// body is closed, so closing an unread body returns nothing more. Counting
// those bytes a second time would send 3 MiB back at the close, and the next
// update would come early.
func TestFirefoxRefreshCloseReturnsNothingTwice(t *testing.T) {
	const first, second = 3 << 20, 2 << 20
	p := newWindowPeer(t, first)
	cc, err := firefoxRefreshTransport().NewClientConn(p.conn)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	res := p.get(t, cc)
	p.waitSent(t, 3, first)
	time.Sleep(50 * time.Millisecond)
	res.Body.Close()
	time.Sleep(100 * time.Millisecond)
	if got := refreshWrites(p.conn); len(got) != 0 {
		t.Fatalf("refresh writes after closing an unread body = %v, want none", got)
	}

	p.setBodySize(second)
	reader := p.lockstep(32 << 14)
	res = p.get(t, cc)
	if n, err := reader.drain(res.Body); err != nil || n != second {
		t.Fatalf("second response: read %d bytes (err %v), want %d", n, err, second)
	}
	res.Body.Close()
	p.waitResponses()
	// 3 MiB, then the first 1 MiB of the second response, make 4 MiB.
	want := [][]peerFrame{{{FrameWindowUpdate, 0, 4194304}}}
	if got := refreshWrites(p.conn); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("refresh writes = %v, want %v", got, want)
	}
}

// A rule measures the connection's window from ConnectionFlow. Without one the
// connection keeps the 15663105 + 65535 it always assumed. With a rule that is
// otherwise the default, a 7 MiB body read in 16 KiB pieces as it arrives
// brings a connection update after more than half of 12 MiB. Without a rule
// it brings none, since half of 15 MiB is never reached. The stream, whose
// window is 12 MiB either way, refreshes at the same read in both.
func TestWindowRefreshSeedsTheConnectionFromConnectionFlow(t *testing.T) {
	const body = 7 << 20
	const crossing = 6291456 + 1<<14 // the first 16 KiB read past half of 12 MiB
	for _, withRule := range []bool{true, false} {
		t.Run(fmt.Sprintf("rule=%v", withRule), func(t *testing.T) {
			p := newWindowPeer(t, body)
			// Kept in step so the stream is still open when it crosses half:
			// no stream update follows the peer's END_STREAM.
			reader := p.lockstep(32 << 14)
			tr := firefoxLikeTransport(true)
			tr.ConnectionFlow = firefoxConnFlow
			if withRule {
				tr.WindowRefresh = &WindowRefresh{Interval: inflowSmallUpdateInterval}
			}
			cc, err := tr.NewClientConn(p.conn)
			if err != nil {
				t.Fatal(err)
			}
			defer cc.Close()

			res := p.get(t, cc)
			if n, err := reader.drain(res.Body); err != nil || n != body {
				t.Fatalf("read %d bytes (err %v), want %d", n, err, body)
			}
			res.Body.Close()
			p.waitResponses()

			want := [][]peerFrame{{{FrameWindowUpdate, 3, crossing}}}
			if withRule {
				want = [][]peerFrame{{{FrameWindowUpdate, 0, crossing}, {FrameWindowUpdate, 3, crossing}}}
			}
			if got := refreshWrites(p.conn); !slices.EqualFunc(got, want, slices.Equal) {
				t.Fatalf("refresh writes = %v, want %v", got, want)
			}
		})
	}
}

// A rule the wire or the accounting cannot carry fails the connection before a
// byte is written.
func TestWindowRefreshRejectsWhatItCannotCarry(t *testing.T) {
	for _, tc := range []struct {
		name string
		tr   *Transport
	}{
		{"AckAt past 31 bits", &Transport{WindowRefresh: &WindowRefresh{AckAt: 1 << 31}}},
		{"Floor past 31 bits", &Transport{WindowRefresh: &WindowRefresh{Floor: 1 << 31}}},
		{"ReadBuffer past 31 bits", &Transport{WindowRefresh: &WindowRefresh{OnArrival: true, ReadBuffer: 1 << 31}}},
		{"negative Interval", &Transport{WindowRefresh: &WindowRefresh{Interval: -time.Second}}},
		{"ReadBuffer without OnArrival", &Transport{WindowRefresh: &WindowRefresh{ReadBuffer: firefoxPipe}}},
		{"connection window past 2^31-1", &Transport{
			ConnectionFlow: math.MaxInt32 - 65534, WindowRefresh: firefoxRefresh(),
		}},
	} {
		c, peer := net.Pipe()
		if _, err := tc.tr.NewClientConn(c); err == nil {
			t.Errorf("%s: NewClientConn succeeded", tc.name)
		}
		if _, err := c.Write([]byte{0}); !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("%s: the connection was left open (write: %v)", tc.name, err)
		}
		peer.Close()
	}
}

// The Transport's rule is copied when a connection is made, so changing it
// later cannot reach a connection that is already reading with it.
func TestWindowRefreshIsCopied(t *testing.T) {
	tr := firefoxRefreshTransport()
	p := newWindowPeer(t, 0)
	cc, err := tr.NewClientConn(p.conn)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	tr.WindowRefresh.AckAt = 1
	if cc.refresh == tr.WindowRefresh || cc.refresh.AckAt != firefoxMinimumToAck {
		t.Fatalf("the connection shares the Transport's rule")
	}
}

// ------------------------------------------------------------------ the peer

// windowPeer is the server end of an in-memory connection that, unlike
// streamPeer, honours the client's windows at both scopes. It answers every
// request with a 200 and a body of bodySize bytes, in 16384-byte DATA frames
// with END_STREAM on the last, and records each frame the client sends.
type windowPeer struct {
	conn *recordingConn // the client's end
	fr   *Framer
	out  chan peerWrite
	quit chan struct{}
	hbuf bytes.Buffer // used only on the writer
	enc  *hpack.Encoder

	mu        sync.Mutex
	cond      *sync.Cond
	bodySize  int
	closed    bool
	initWin   int64
	connWin   int64
	streamWin map[uint32]int64
	reset     map[uint32]bool
	sent      map[uint32]int
	pending   int                        // responses not yet fully written
	pace      func(stream uint32, n int) // runs after each DATA frame; may block
}

func newWindowPeer(t *testing.T, bodySize int) *windowPeer {
	t.Helper()
	cli, srv := net.Pipe()
	p := &windowPeer{
		conn: &recordingConn{Conn: cli}, fr: NewFramer(srv, srv),
		out: make(chan peerWrite, 64), quit: make(chan struct{}),
		bodySize: bodySize, initWin: 65535, connWin: 65535,
		streamWin: map[uint32]int64{}, reset: map[uint32]bool{}, sent: map[uint32]int{},
	}
	p.cond = sync.NewCond(&p.mu)
	p.enc = hpack.NewEncoder(&p.hbuf)
	t.Cleanup(func() {
		close(p.quit)
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		p.cond.Broadcast()
		cli.Close()
		srv.Close()
	})
	go p.writer()
	go p.serve(srv)
	return p
}

func (p *windowPeer) setBodySize(n int) {
	p.mu.Lock()
	p.bodySize = n
	p.mu.Unlock()
}

// get sends a GET on cc and returns the response, its body unread.
func (p *windowPeer) get(t *testing.T, cc *ClientConn) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", "https://peer.test/", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cc.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (p *windowPeer) sentOn(id uint32) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent[id]
}

// waitSent waits until the peer has written n body bytes on stream id.
func (p *windowPeer) waitSent(t *testing.T, id uint32, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for p.sentOn(id) < n {
		if time.Now().After(deadline) {
			t.Fatalf("peer sent %d bytes on stream %d, want %d", p.sentOn(id), id, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitResponses waits until every response the peer started is fully written.
func (p *windowPeer) waitResponses() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.pending > 0 && !p.closed {
		p.cond.Wait()
	}
}

// lockstep makes the peer stop after every step bytes on a stream until the
// returned reader has read everything sent so far. The client then never
// holds more than step bytes unread when a frame arrives.
func (p *windowPeer) lockstep(step int) *stepReader {
	r := &stepReader{}
	r.cond = sync.NewCond(&r.mu)
	p.mu.Lock()
	p.pace = func(_ uint32, sent int) {
		if sent%step != 0 {
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		for r.n < sent && !r.done {
			r.cond.Wait()
		}
	}
	p.mu.Unlock()
	return r
}

// stepReader drains a body and reports its progress to a lockstep peer.
type stepReader struct {
	mu   sync.Mutex
	cond *sync.Cond
	n    int
	done bool
}

func (r *stepReader) drain(body io.Reader) (int, error) {
	defer func() {
		r.mu.Lock()
		r.done = true
		r.mu.Unlock()
		r.cond.Broadcast()
	}()
	buf := make([]byte, 1<<14)
	for {
		n, err := body.Read(buf)
		r.mu.Lock()
		r.n += n
		total := r.n
		r.mu.Unlock()
		r.cond.Broadcast()
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

func (p *windowPeer) serve(c net.Conn) {
	if _, err := io.ReadFull(c, make([]byte, len(ClientPreface))); err != nil {
		return
	}
	p.send(func() error { return p.fr.WriteSettings() })
	defer func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		p.cond.Broadcast()
	}()
	for {
		f, err := p.fr.ReadFrame()
		if err != nil {
			return
		}
		ended := false
		p.mu.Lock()
		switch f := f.(type) {
		case *SettingsFrame:
			if !f.IsAck() {
				if v, ok := f.Value(SettingInitialWindowSize); ok {
					delta := int64(v) - p.initWin
					p.initWin = int64(v)
					for id := range p.streamWin {
						p.streamWin[id] += delta
					}
				}
				p.send(func() error { return p.fr.WriteSettingsAck() })
			}
		case *WindowUpdateFrame:
			if f.StreamID == 0 {
				p.connWin += int64(f.Increment)
			} else {
				p.streamWin[f.StreamID] += int64(f.Increment)
			}
		case *HeadersFrame:
			if _, ok := p.streamWin[f.StreamID]; !ok {
				p.streamWin[f.StreamID] = p.initWin
			}
			ended = f.StreamEnded()
		case *RSTStreamFrame:
			p.reset[f.StreamID] = true
		}
		if ended {
			p.pending++
		}
		p.mu.Unlock()
		p.cond.Broadcast()
		if ended {
			go p.respond(f.Header().StreamID)
		}
	}
}

// credit waits until both windows allow at least one byte and takes up to
// want. It returns 0 once the connection or the stream has gone.
func (p *windowPeer) credit(id uint32, want int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if p.closed || p.reset[id] {
			return 0
		}
		if avail := min(p.connWin, p.streamWin[id]); avail > 0 {
			n := min(int64(want), avail)
			p.connWin -= n
			p.streamWin[id] -= n
			return int(n)
		}
		p.cond.Wait()
	}
}

func (p *windowPeer) respond(id uint32) {
	defer func() {
		p.mu.Lock()
		p.pending--
		p.mu.Unlock()
		p.cond.Broadcast()
	}()
	p.mu.Lock()
	size, pace := p.bodySize, p.pace
	p.mu.Unlock()
	p.write(func() error {
		p.hbuf.Reset()
		p.enc.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
		p.enc.WriteField(hpack.HeaderField{Name: "content-length", Value: strconv.Itoa(size)})
		return p.fr.WriteHeaders(HeadersFrameParam{
			StreamID: id, BlockFragment: p.hbuf.Bytes(),
			EndHeaders: true, EndStream: size == 0,
		})
	})
	chunk := make([]byte, 1<<14)
	for sent := 0; sent < size; {
		n := p.credit(id, min(len(chunk), size-sent))
		if n == 0 {
			return
		}
		sent += n
		end := sent == size
		if err := p.write(func() error { return p.fr.WriteData(id, end, chunk[:n]) }); err != nil {
			return
		}
		p.mu.Lock()
		p.sent[id] = sent
		p.mu.Unlock()
		p.cond.Broadcast()
		if pace != nil {
			pace(id, sent)
		}
	}
}

func (p *windowPeer) writer() {
	for {
		select {
		case w := <-p.out:
			err := w.f()
			if w.done != nil {
				w.done <- err
			}
		case <-p.quit:
			return
		}
	}
}

// send queues a frame and returns at once, which is all serve may do.
func (p *windowPeer) send(f func() error) {
	select {
	case p.out <- peerWrite{f: f}:
	case <-p.quit:
	}
}

// write queues a frame and waits until it has been written.
func (p *windowPeer) write(f func() error) error {
	done := make(chan error, 1)
	select {
	case p.out <- peerWrite{f: f, done: done}:
	case <-p.quit:
		return net.ErrClosed
	}
	select {
	case err := <-done:
		return err
	case <-p.quit:
		return net.ErrClosed
	}
}

// refreshWrites returns the WINDOW_UPDATE frames of each write the client made
// that carried any, other than the writes that open the connection or a
// stream: those carry SETTINGS or HEADERS too.
func refreshWrites(c *recordingConn) [][]peerFrame {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out [][]peerFrame
	for _, w := range c.writes {
		w = bytes.TrimPrefix(w, []byte(ClientPreface))
		var updates []peerFrame
		opening := false
		for len(w) >= frameHeaderLen {
			n := int(w[0])<<16 | int(w[1])<<8 | int(w[2])
			if len(w) < frameHeaderLen+n {
				break
			}
			typ := FrameType(w[3])
			switch typ {
			case FrameSettings, FrameHeaders:
				opening = true
			case FrameWindowUpdate:
				updates = append(updates, peerFrame{
					typ:    typ,
					stream: binary.BigEndian.Uint32(w[5:9]) & (1<<31 - 1),
					val:    binary.BigEndian.Uint32(w[frameHeaderLen:]) & (1<<31 - 1),
				})
			}
			w = w[frameHeaderLen+n:]
		}
		if len(updates) > 0 && !opening {
			out = append(out, updates)
		}
	}
	return out
}
