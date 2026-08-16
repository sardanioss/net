package hpack

import (
	"bytes"
	"strings"
	"testing"
)

// Locks the two Chrome-parity deviations this fork carries. Both are invisible
// to any test that compares decoded headers: the header list is identical
// either way, only the wire instructions differ.

// IndexingChrome is a port of quiche HpackEncoder's DefaultPolicy. Pseudo
// headers are not indexed EXCEPT :authority; every regular header is, with no
// exclusion list and no value-size threshold.
func TestChromeIndexingPolicyMatchesQuiche(t *testing.T) {
	e := NewEncoder(&bytes.Buffer{})
	e.SetIndexingPolicy(IndexingChrome)

	cases := []struct {
		name, value string
		want        bool
		why         string
	}{
		{":authority", "www.example.com", true, "the one pseudo-header Chrome indexes"},
		{":method", "GET", false, "pseudo, not :authority"},
		{":path", "/a", false, "pseudo, not :authority"},
		{":scheme", "https", false, "pseudo, not :authority"},
		{"cookie", "a=1", true, "Chrome indexes cookie crumbs; excluding them re-sends the whole jar every request"},
		{"authorization", "Bearer x", true, "Chrome indexes it despite RFC 7541 7.1.3"},
		{"proxy-authorization", "Basic x", true, "same"},
		{"user-agent", "Mozilla/5.0", true, "regular header"},
		{"x-anything", "v", true, "no allow-list: every regular header is indexed"},
		{"cookie", "_abck=" + strings.Repeat("A", 480), true, "no size threshold; Chrome inserts a 480-char crumb"},
	}
	for _, c := range cases {
		if got := e.shouldIndex(HeaderField{Name: c.name, Value: c.value}); got != c.want {
			t.Errorf("shouldIndex(%s) = %v, want %v (%s)", c.name, got, c.want, c.why)
		}
	}
}

// The static name map is first-wins, matching quiche hpack_static_table.cc.
// Last-wins puts :method: POST and :path: /index.html on the wire as the name
// reference where Chrome puts :method: GET and :path: /.
func TestStaticNameIndexIsFirstWins(t *testing.T) {
	for _, c := range []struct {
		name string
		want uint64
	}{
		{":method", 2}, {":path", 4}, {":scheme", 6}, {":status", 8}, {":authority", 1},
	} {
		if got := staticTable.byName[c.name]; got != c.want {
			t.Errorf("byName[%q] = %d, want %d (first-wins, per quiche)", c.name, got, c.want)
		}
	}
}

// End to end on the bytes, which is the only formulation that catches this.
func TestChromeWireInstructions(t *testing.T) {
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	e.SetIndexingPolicy(IndexingChrome)

	// :authority must be 0x41 = literal WITH incremental indexing, name index 1.
	buf.Reset()
	e.WriteField(HeaderField{Name: ":authority", Value: "www.tui.fi"})
	if b := buf.Bytes(); len(b) == 0 || b[0] != 0x41 {
		t.Errorf(":authority first byte = %#x, want 0x41 (Chrome indexes it; 0x01 means it is "+
			"re-sent as a literal on every request for the life of the connection)", buf.Bytes()[0])
	}

	// A non-root :path must be 0x04 = literal WITHOUT indexing, name index 4.
	buf.Reset()
	e.WriteField(HeaderField{Name: ":path", Value: "/fi/matkat"})
	if b := buf.Bytes(); len(b) == 0 || b[0] != 0x04 {
		t.Errorf(":path first byte = %#x, want 0x04 (name index 4 = \":path: /\"; 0x05 references "+
			"\":path: /index.html\")", buf.Bytes()[0])
	}

	// A non-GET/POST :method must be 0x02, name index 2, value raw.
	buf.Reset()
	e.WriteField(HeaderField{Name: ":method", Value: "PUT"})
	if b := buf.Bytes(); len(b) < 2 || b[0] != 0x02 {
		t.Errorf(":method first byte = %#x, want 0x02", buf.Bytes()[0])
	} else if b[1]&0x80 != 0 {
		t.Error(":method value was Huffman-coded; Chrome emits it raw, since Huffman is used " +
			"only when strictly smaller and \"PUT\" is not")
	}

	// A cookie crumb must be 0x60 = literal WITH incremental indexing, name index 32.
	buf.Reset()
	e.WriteField(HeaderField{Name: "cookie", Value: "alpha=one"})
	if b := buf.Bytes(); len(b) == 0 || b[0] != 0x60 {
		t.Errorf("cookie first byte = %#x, want 0x60 (0x1f11 is never-indexed, which no browser "+
			"emits and which stops the crumb ever entering the table)", buf.Bytes()[0])
	}
}
