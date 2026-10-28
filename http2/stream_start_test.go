package http2

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	http "github.com/sardanioss/http"
	"github.com/sardanioss/net/http2/hpack"
	tls "github.com/sardanioss/utls"
)

// FirstStreamID and StreamWindowUpdate, end to end over an in-memory
// connection.
//
// Firefox opens client streams from 3, and follows every HEADERS with a
// WINDOW_UPDATE that takes the stream from the 131072 it advertised to 12 MiB.
// These pin the frames, the write they leave in, and the receive window the
// client then enforces. They also pin that a connection keeps the values it
// was made with, and that a GOAWAY treats its first stream as the first
// wherever it starts.
//
// The package's other test files do not build against the forked net/http, so
// this one runs on its own:
//
//	go test $(go list -f '{{join .GoFiles " "}}' .) stream_start_test.go

const (
	firefoxPushAllowance = 131072   // network.http.http2.push-allowance
	firefoxStreamBump    = 12451840 // pull-allowance 12582912 minus the above
)

// firefoxLikeTransport advertises Firefox's initial window and, with bump,
// opens its streams the way Firefox does.
func firefoxLikeTransport(bump bool) *Transport {
	t := &Transport{
		Settings:      map[SettingID]uint32{SettingInitialWindowSize: firefoxPushAllowance},
		SettingsOrder: []SettingID{SettingInitialWindowSize},
	}
	if bump {
		t.FirstStreamID = 3
		t.StreamWindowUpdate = firefoxStreamBump
	}
	return t
}

func TestFirstStreamID(t *testing.T) {
	for _, tc := range []struct {
		first uint32
		want  []uint32
	}{
		{0, []uint32{1, 3, 5}},
		{3, []uint32{3, 5, 7}},
	} {
		p := newStreamPeer(t, 0)
		cc, err := (&Transport{FirstStreamID: tc.first}).NewClientConn(p.conn)
		if err != nil {
			t.Fatal(err)
		}
		for range tc.want {
			if _, err := p.roundTrip(cc, "GET", nil); err != nil {
				t.Fatal(err)
			}
		}
		cc.Close()
		if got := p.streamsOf(FrameHeaders); !slices.Equal(got, tc.want) {
			t.Errorf("FirstStreamID %d: HEADERS on streams %v, want %v", tc.first, got, tc.want)
		}
	}
}

// A GOAWAY with an error code, from a server that has processed no stream yet,
// fails the connection's first request outright: a server that refuses a new
// connection would likely refuse the next one too. That is upstream's rule for
// stream 1. Any later stream is still retried on a new connection. With
// FirstStreamID 3 the first stream is 3, and a check for stream 1 would retry
// it instead.
func TestGoAwayErrorFailsTheFirstStream(t *testing.T) {
	errRedial := errors.New("dialed a second connection")
	for _, first := range []uint32{0, 3} {
		p := newStreamPeer(t, 0)
		p.goAwayAfter, p.goAwayCode = 2, ErrCodeInternal
		var dialed atomic.Bool
		tr := &Transport{
			FirstStreamID: first,
			// The first dial gets the peer. A retry dials again, and fails
			// there, so its error shows that the request was retried.
			DialTLSContext: func(context.Context, string, string, *tls.Config) (net.Conn, error) {
				if dialed.Swap(true) {
					return nil, errRedial
				}
				return p.conn, nil
			},
		}

		var (
			errs [2]error
			wg   sync.WaitGroup
		)
		for i := range errs {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "GET", "https://peer.test/", nil)
			if err != nil {
				t.Fatal(err)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = tr.RoundTrip(req)
			}()
			// Its HEADERS have arrived, so the next request takes the next
			// stream.
			p.waitFor(t, FrameHeaders, i+1)
		}
		wg.Wait()
		tr.CloseIdleConnections()

		ids := p.streamsOf(FrameHeaders)
		if errs[0] == nil || errors.Is(errs[0], errRedial) {
			t.Errorf("FirstStreamID %d: the first request, on stream %d, got %v; want the GOAWAY's error, with no retry",
				first, ids[0], errs[0])
		}
		if !errors.Is(errs[1], errRedial) {
			t.Errorf("FirstStreamID %d: the second request, on stream %d, got %v; want it retried on a new connection",
				first, ids[1], errs[1])
		}
	}
}

// A value the wire cannot carry fails the connection before a byte is written,
// rather than becoming a protocol error on the first request.
func TestStreamStartRejectsWhatTheWireCannotCarry(t *testing.T) {
	for _, tc := range []struct {
		name string
		tr   *Transport
	}{
		{"even first stream", &Transport{FirstStreamID: 2}},
		{"first stream past 31 bits", &Transport{FirstStreamID: 1<<31 + 1}},
		{"window past 2^31-1", &Transport{
			Settings:           map[SettingID]uint32{SettingInitialWindowSize: firefoxPushAllowance},
			StreamWindowUpdate: math.MaxInt32,
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

// Each stream's HEADERS is followed by its WINDOW_UPDATE before anything else
// on that stream, a POST's DATA included, and the two go out in one write, as
// Firefox's AdjustInitialWindow appends the update to the HEADERS it is
// sending.
func TestStreamWindowUpdateFollowsHeaders(t *testing.T) {
	p := newStreamPeer(t, 0)
	cc, err := firefoxLikeTransport(true).NewClientConn(p.conn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.roundTrip(cc, "GET", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.roundTrip(cc, "POST", []byte("k=1")); err != nil {
		t.Fatal(err)
	}
	cc.Close()

	var got []peerFrame
	for _, f := range p.recorded() {
		if f.stream != 0 {
			got = append(got, f)
		}
	}
	want := []peerFrame{
		{FrameHeaders, 3, 0},
		{FrameWindowUpdate, 3, firefoxStreamBump},
		{FrameHeaders, 5, 0},
		{FrameWindowUpdate, 5, firefoxStreamBump},
		{FrameData, 5, 0},
	}
	if len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
		t.Fatalf("stream frames = %v, want them to start %v", got, want)
	}
	for _, f := range got[len(want):] {
		if f.typ != FrameData {
			t.Errorf("unexpected %v on stream %d after the request was sent", f.typ, f.stream)
		}
	}

	for _, id := range []uint32{3, 5} {
		w := p.conn.writeWith(FrameHeaders, id)
		if !slices.Equal(w, []peerFrame{{FrameHeaders, id, 0}, {FrameWindowUpdate, id, firefoxStreamBump}}) {
			t.Errorf("stream %d: the write carrying its HEADERS held %v, want HEADERS then WINDOW_UPDATE", id, w)
		}
	}
}

// The update is credit the peer may spend at once. A peer that fills the
// window before the client reads a byte stays inside it with the update and
// overruns it without, which must fail the connection with FLOW_CONTROL_ERROR.
// (The read loop queues a GOAWAY for that but closes without flushing it, as
// upstream does, so the error is asserted on the client side.)
func TestStreamWindowUpdateCreditsTheStream(t *testing.T) {
	const body = firefoxPushAllowance + 16384 // one frame past the advertised window
	for _, bump := range []bool{true, false} {
		p := newStreamPeer(t, body)
		cc, err := firefoxLikeTransport(bump).NewClientConn(p.conn)
		if err != nil {
			t.Fatal(err)
		}
		got, err := p.roundTrip(cc, "GET", nil)
		cc.Close()
		if bump {
			if err != nil || len(got) != body {
				t.Errorf("with the update: read %d bytes, err %v; want all %d", len(got), err, body)
			}
			for _, f := range p.recorded() {
				if f.typ == FrameWindowUpdate && f.stream != 0 && f.val != firefoxStreamBump {
					t.Errorf("with the update: stream %d refreshed by %d, though %d bytes is far from half of 12 MiB",
						f.stream, f.val, body)
				}
			}
			continue
		}
		var ce ConnectionError
		if !errors.As(err, &ce) || ErrCode(ce) != ErrCodeFlowControl {
			t.Errorf("without the update: read %d bytes, err %v; want FLOW_CONTROL_ERROR past the advertised window",
				len(got), err)
		}
	}
}

// A connection reads StreamWindowUpdate when it is made, as it sizes its
// stream windows then, so changing the Transport later reaches neither. Read
// when each stream opens, the update would announce a window other than the
// one the client enforces and refreshes.
func TestStreamWindowUpdateIsCopied(t *testing.T) {
	for _, tc := range []struct {
		made, later uint32
		want        []peerFrame
	}{
		{firefoxStreamBump, 0, []peerFrame{{FrameHeaders, 1, 0}, {FrameWindowUpdate, 1, firefoxStreamBump}}},
		{0, firefoxStreamBump, []peerFrame{{FrameHeaders, 1, 0}}},
	} {
		p := newStreamPeer(t, 0)
		tr := firefoxLikeTransport(false)
		tr.StreamWindowUpdate = tc.made
		cc, err := tr.NewClientConn(p.conn)
		if err != nil {
			t.Fatal(err)
		}
		tr.StreamWindowUpdate = tc.later
		if _, err := p.roundTrip(cc, "GET", nil); err != nil {
			t.Fatal(err)
		}
		cc.Close()
		if got := p.conn.writeWith(FrameHeaders, 1); !slices.Equal(got, tc.want) {
			t.Errorf("made with %d, then set to %d: the write carrying HEADERS held %v, want %v",
				tc.made, tc.later, got, tc.want)
		}
	}
}

// ------------------------------------------------------------------ the peer

// peerFrame is one frame the client sent: its type, its stream, and the
// increment if it is a WINDOW_UPDATE.
type peerFrame struct {
	typ    FrameType
	stream uint32
	val    uint32
}

// streamPeer is the server end of an in-memory connection. It answers every
// request with a 200 and a body of bodySize bytes, written straight out with
// no regard for the client's windows, and records each frame the client sends.
type streamPeer struct {
	conn     *recordingConn // the client's end
	fr       *Framer
	bodySize int
	writing  sync.WaitGroup // responses not yet fully written

	// goAwayAfter, set before the first request, makes the peer answer no
	// request. Once that many have arrived, it sends a GOAWAY with goAwayCode
	// and last stream ID 0, as a server that processed none of them would.
	goAwayAfter int
	goAwayCode  ErrCode

	// Every frame the peer sends goes through one writer, in order. serve
	// never writes itself: a pipe has no buffer, so a peer that stops reading
	// to write can meet a client doing the same and neither moves.
	out  chan peerWrite
	quit chan struct{}
	hbuf bytes.Buffer // used only on the writer
	enc  *hpack.Encoder

	mu     sync.Mutex
	frames []peerFrame
}

type peerWrite struct {
	f    func() error
	done chan error // nil when nobody waits for the result
}

func newStreamPeer(t *testing.T, bodySize int) *streamPeer {
	t.Helper()
	cli, srv := net.Pipe()
	p := &streamPeer{
		conn: &recordingConn{Conn: cli}, fr: NewFramer(srv, srv), bodySize: bodySize,
		out: make(chan peerWrite, 64), quit: make(chan struct{}),
	}
	p.enc = hpack.NewEncoder(&p.hbuf)
	t.Cleanup(func() {
		close(p.quit)
		cli.Close()
		srv.Close()
	})
	go p.writer()
	go p.serve(srv)
	return p
}

// roundTrip sends one request on cc and reads the whole response body. The
// body is read only once the peer has written all of it, so nothing the client
// consumes can refresh a window while the peer is still sending.
func (p *streamPeer) roundTrip(cc *ClientConn, method string, body []byte) ([]byte, error) {
	var rb io.Reader
	if body != nil {
		rb = bytes.NewReader(body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "https://peer.test/", rb)
	if err != nil {
		return nil, err
	}
	res, err := cc.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	p.writing.Wait()
	return io.ReadAll(res.Body)
}

func (p *streamPeer) serve(c net.Conn) {
	if _, err := io.ReadFull(c, make([]byte, len(ClientPreface))); err != nil {
		return
	}
	p.send(func() error { return p.fr.WriteSettings() })
	requests := 0
	for {
		f, err := p.fr.ReadFrame()
		if err != nil {
			return
		}
		rec := peerFrame{typ: f.Header().Type, stream: f.Header().StreamID}
		ended := false
		switch f := f.(type) {
		case *SettingsFrame:
			if !f.IsAck() {
				p.send(func() error { return p.fr.WriteSettingsAck() })
			}
		case *WindowUpdateFrame:
			rec.val = f.Increment
		case *HeadersFrame:
			ended = f.StreamEnded()
		case *DataFrame:
			ended = f.StreamEnded()
		}
		p.mu.Lock()
		p.frames = append(p.frames, rec)
		p.mu.Unlock()
		if !ended {
			continue
		}
		if p.goAwayAfter > 0 {
			if requests++; requests == p.goAwayAfter {
				p.send(func() error { return p.fr.WriteGoAway(0, p.goAwayCode, nil) })
			}
			continue
		}
		// Counted here rather than in respond, so a round trip that has its
		// response headers is sure to wait for the rest.
		p.writing.Add(1)
		go p.respond(rec.stream)
	}
}

func (p *streamPeer) respond(id uint32) {
	defer p.writing.Done()
	p.write(func() error {
		p.hbuf.Reset()
		p.enc.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
		return p.fr.WriteHeaders(HeadersFrameParam{
			StreamID: id, BlockFragment: p.hbuf.Bytes(),
			EndHeaders: true, EndStream: p.bodySize == 0,
		})
	})
	chunk := make([]byte, 16384)
	for n := 0; n < p.bodySize; {
		size := min(len(chunk), p.bodySize-n)
		n += size
		end := n == p.bodySize
		if err := p.write(func() error { return p.fr.WriteData(id, end, chunk[:size]) }); err != nil {
			return // the client hung up, which is what an overrun earns
		}
	}
}

func (p *streamPeer) writer() {
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
func (p *streamPeer) send(f func() error) {
	select {
	case p.out <- peerWrite{f: f}:
	case <-p.quit:
	}
}

// write queues a frame and waits until it has been written.
func (p *streamPeer) write(f func() error) error {
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

func (p *streamPeer) recorded() []peerFrame {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.frames)
}

func (p *streamPeer) streamsOf(typ FrameType) []uint32 {
	var out []uint32
	for _, f := range p.recorded() {
		if f.typ == typ {
			out = append(out, f.stream)
		}
	}
	return out
}

// waitFor waits until the peer has seen n frames of the given type.
func (p *streamPeer) waitFor(t *testing.T, typ FrameType, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(p.streamsOf(typ)) < n {
		if time.Now().After(deadline) {
			t.Fatalf("the peer saw %d %v frames, want %d", len(p.streamsOf(typ)), typ, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// recordingConn keeps every Write the client makes, so a test can see which
// frames left together.
type recordingConn struct {
	net.Conn
	mu     sync.Mutex
	writes [][]byte
}

func (c *recordingConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.writes = append(c.writes, slices.Clone(b))
	c.mu.Unlock()
	return c.Conn.Write(b)
}

// writeWith returns the frames of the first write that carried a frame of the
// given type on the given stream.
func (c *recordingConn) writeWith(typ FrameType, stream uint32) []peerFrame {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, w := range c.writes {
		w = bytes.TrimPrefix(w, []byte(ClientPreface))
		var frames []peerFrame
		found := false
		for len(w) >= frameHeaderLen {
			n := int(w[0])<<16 | int(w[1])<<8 | int(w[2])
			if len(w) < frameHeaderLen+n {
				break
			}
			f := peerFrame{typ: FrameType(w[3]), stream: binary.BigEndian.Uint32(w[5:9]) & (1<<31 - 1)}
			if f.typ == FrameWindowUpdate && n == 4 {
				f.val = binary.BigEndian.Uint32(w[frameHeaderLen:]) & (1<<31 - 1)
			}
			if f.typ == typ && f.stream == stream {
				found = true
			}
			frames = append(frames, f)
			w = w[frameHeaderLen+n:]
		}
		if found {
			return frames
		}
	}
	return nil
}
