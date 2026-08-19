package hpack

import (
	"bytes"
	"testing"
)

// repr names the HPACK representation a field went out as, read from the
// prefix bits of the first byte (RFC 7541 6.1 - 6.2.3).
func repr(t *testing.T, b []byte) string {
	t.Helper()
	if len(b) == 0 {
		t.Fatal("no bytes emitted")
	}
	switch {
	case b[0]&0x80 != 0:
		return "indexed" // 1xxxxxxx, 6.1
	case b[0]&0xC0 == 0x40:
		return "incremental" // 01xxxxxx, 6.2.1
	case b[0]&0xF0 == 0x10:
		return "never-indexed" // 0001xxxx, 6.2.3
	case b[0]&0xF0 == 0x00:
		return "without-indexing" // 0000xxxx, 6.2.2
	default:
		return "table-size-update"
	}
}

func encodeOne(t *testing.T, e *Encoder, buf *bytes.Buffer, f HeaderField) []byte {
	t.Helper()
	buf.Reset()
	if err := e.WriteField(f); err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), buf.Bytes()...)
}

// SetNeverIndexHeaders promised the never-indexed representation and delivered
// "without indexing". The 0x10 prefix comes from HeaderField.Sensitive, which
// nothing on the write path ever set, so the list only reached shouldIndex and
// the byte on the wire was 0x00.
func TestNeverIndexEmitsNeverIndexedRepresentation(t *testing.T) {
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	e.SetNeverIndexHeaders([]string{"cookie"})

	got := repr(t, encodeOne(t, e, &buf, HeaderField{Name: "cookie", Value: "a=1"}))
	if got != "never-indexed" {
		t.Errorf("cookie went out as %s, want never-indexed (0x10 prefix)", got)
	}
}

// RFC 7541 7.1.3: a never-indexed field must use the literal never-indexed
// form. searchTable ran before the indexing decision, so a field whose name
// AND value both matched a table entry returned an indexed reference (0x80)
// without shouldIndex ever being consulted. accept-encoding: gzip, deflate is
// static index 16, so it exercised that path exactly.
func TestNeverIndexBeatsAnExactTableMatch(t *testing.T) {
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	e.SetNeverIndexHeaders([]string{"accept-encoding"})

	f := HeaderField{Name: "accept-encoding", Value: "gzip, deflate"}
	got := repr(t, encodeOne(t, e, &buf, f))
	if got != "never-indexed" {
		t.Errorf("exact static-table match went out as %s, want never-indexed", got)
	}
}

// A never-indexed field is never added to the dynamic table, so sending it
// twice must produce the same representation both times. If the first one
// leaked into the table, the second would come back as an indexed reference.
func TestNeverIndexNeverEntersTheDynamicTable(t *testing.T) {
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	e.SetNeverIndexHeaders([]string{"authorization"})

	f := HeaderField{Name: "authorization", Value: "Bearer xyz"}
	first := repr(t, encodeOne(t, e, &buf, f))
	second := repr(t, encodeOne(t, e, &buf, f))
	if first != "never-indexed" || second != "never-indexed" {
		t.Errorf("representations were %s then %s, want never-indexed twice", first, second)
	}
}

// never-index and always-index contradict each other. The one with a security
// meaning wins, and it has to win over CustomIndexingFunc too, which sits in
// front of both lists inside shouldIndex.
func TestNeverIndexOutranksTheOtherIndexingControls(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*Encoder)
	}{
		{"always-index list", func(e *Encoder) { e.SetAlwaysIndexHeaders([]string{"cookie"}) }},
		{"custom indexing func", func(e *Encoder) {
			e.SetCustomIndexingFunc(func(HeaderField) bool { return true })
		}},
	} {
		var buf bytes.Buffer
		e := NewEncoder(&buf)
		e.SetNeverIndexHeaders([]string{"cookie"})
		tc.setup(e)

		got := repr(t, encodeOne(t, e, &buf, HeaderField{Name: "cookie", Value: "a=1"}))
		if got != "never-indexed" {
			t.Errorf("with %s set, cookie went out as %s, want never-indexed", tc.name, got)
		}
	}
}

// Guards the 1.6.11 fix. Chrome sets no never-index list at all and indexes
// cookie like any other header, so an empty list must still produce
// incremental indexing. A regression here re-inflates every request by the
// size of the jar.
func TestEmptyNeverIndexListStillIndexesCookie(t *testing.T) {
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	e.SetNeverIndexHeaders(nil)

	got := repr(t, encodeOne(t, e, &buf, HeaderField{Name: "cookie", Value: "a=1"}))
	if got != "incremental" {
		t.Errorf("cookie went out as %s with no never-index list, want incremental", got)
	}
}
