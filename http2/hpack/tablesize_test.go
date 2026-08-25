package hpack

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// The dynamic table size update the encoder writes back when a peer advertises
// SETTINGS_HEADER_TABLE_SIZE.
//
// The model is quiche's HpackEncoder, which is what a browser runs:
//
//	void HpackEncoder::ApplyHeaderTableSizeSetting(size_t size_setting) {
//	  if (size_setting == header_table_.settings_size_bound() &&
//	      size_setting <= table_size_upper_bound_) {
//	    return;
//	  }
//	  if (size_setting < header_table_.settings_size_bound()) {
//	    min_table_size_setting_received_ =
//	        std::min(size_setting, min_table_size_setting_received_);
//	  }
//	  header_table_.SetSettingsHeaderTableSize(size_setting);
//	  if (size_setting > table_size_upper_bound_) {
//	    header_table_.SetMaxSize(table_size_upper_bound_);
//	  }
//	  should_emit_table_size_ = true;
//	}
//
// Every row here is a value the SERVER picks, so every divergence is an active
// probe rather than something the client happens to reveal.

// emit runs a sequence of advertised sizes through an encoder and returns the
// bytes of the following header block.
func emit(t *testing.T, limit uint32, advertised ...uint32) string {
	t.Helper()
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	e.SetMaxDynamicTableSizeLimit(limit)
	for _, v := range advertised {
		e.SetMaxDynamicTableSize(v)
	}
	if err := e.WriteField(HeaderField{Name: ":method", Value: "GET"}); err != nil {
		t.Fatalf("WriteField: %v", err)
	}
	// The size update is a prefix; drop the trailing field so the rows read as
	// just the updates. :method GET is static index 2, one byte, 0x82.
	b := buf.Bytes()
	if len(b) == 0 || b[len(b)-1] != 0x82 {
		t.Fatalf("unexpected trailing field bytes %x", b)
	}
	return hex.EncodeToString(b[:len(b)-1])
}

func TestEncoderTableSizeUpdateMatchesQuiche(t *testing.T) {
	const noClamp = 1 << 24 // well above every value here

	for _, tc := range []struct {
		name       string
		limit      uint32
		advertised []uint32
		want       string
		why        string
	}{
		{
			name:       "default size is a no-op",
			limit:      noClamp,
			advertised: []uint32{4096},
			want:       "",
			why: "4096 is already the bound, so there is nothing to tell the " +
				"peer. Every Go HTTP/2 server advertises exactly this.",
		},
		{
			name:       "a larger size is echoed",
			limit:      noClamp,
			advertised: []uint32{65536},
			want:       "3fe1ff03",
		},
		{
			name:       "past our own advertised size is still echoed",
			limit:      noClamp,
			advertised: []uint32{65537},
			want:       "3fe2ff03",
			why: "The encoder limit governs the other direction of the " +
				"connection and has no business clamping what the peer said.",
		},
		{
			name:       "a megabyte is echoed",
			limit:      noClamp,
			advertised: []uint32{1048576},
			want:       "3fe1ff3f",
		},
		{
			name:       "two increasing sizes emit one update",
			limit:      noClamp,
			advertised: []uint32{8192, 16384},
			want:       "3fe17f",
			why: "Neither is below the bound at the time it arrives, so the " +
				"minimum is never lowered and only the final size goes out.",
		},
		{
			name:       "a decrease emits the minimum first",
			limit:      noClamp,
			advertised: []uint32{16384, 8192},
			want:       "3fe13f",
			why: "8192 is below the bound, so it becomes the minimum, and it " +
				"is also the target, so one update carries both.",
		},
		{
			name:       "down then up emits both",
			limit:      noClamp,
			advertised: []uint32{16384, 4096, 16384},
			want:       "3fe11f3fe17f",
			why: "RFC 7541 requires the smallest size seen to be signalled " +
				"before the final one.",
		},
		{
			name:       "repeating a size is a no-op",
			limit:      noClamp,
			advertised: []uint32{65536, 65536},
			want:       "3fe1ff03",
		},
		{
			name:       "the limit clamps the table but not the echo",
			limit:      8192,
			advertised: []uint32{65536},
			want:       "3fe13f",
			why: "The table cannot exceed the limit, so the update carries " +
				"the clamped target.",
		},
		{
			name:       "repeating a clamped size still re-arms",
			limit:      8192,
			advertised: []uint32{65536, 65536},
			want:       "3fe13f",
			why: "The bound records 65536 and the second call matches it, but " +
				"the second half of the early return (still above the limit) " +
				"is what keeps this from silently doing nothing.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := emit(t, tc.limit, tc.advertised...)
			if got != tc.want {
				t.Errorf("advertised %v with limit %d\n got %q\nwant %q\n%s",
					tc.advertised, tc.limit, got, tc.want, tc.why)
			}
		})
	}
}

// A second header block on the same encoder emits no further update. The
// emitter clears its flag and resets the minimum, and that half was already a
// faithful port; this pins it so a change to the setter cannot quietly make
// every block carry a prefix.
func TestEncoderTableSizeUpdateEmittedOnce(t *testing.T) {
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	e.SetMaxDynamicTableSizeLimit(1 << 24)
	e.SetMaxDynamicTableSize(65536)

	e.WriteField(HeaderField{Name: ":method", Value: "GET"})
	first := hex.EncodeToString(buf.Bytes())
	buf.Reset()
	e.WriteField(HeaderField{Name: ":method", Value: "GET"})
	second := hex.EncodeToString(buf.Bytes())

	if first != "3fe1ff0382" {
		t.Errorf("first block = %s, want 3fe1ff0382", first)
	}
	if second != "82" {
		t.Errorf("second block = %s, want 82 with no size update", second)
	}
}

// Raising the limit after the peer sequence has to move the table up too, or
// the encoder keeps using a smaller table than it has told the peer about.
func TestEncoderRaisingTheLimitReopensTheTable(t *testing.T) {
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	e.SetMaxDynamicTableSizeLimit(8192)
	e.SetMaxDynamicTableSize(65536)
	e.WriteField(HeaderField{Name: ":method", Value: "GET"})
	if got := hex.EncodeToString(buf.Bytes()); got != "3fe13f82" {
		t.Fatalf("clamped block = %s, want 3fe13f82", got)
	}

	buf.Reset()
	e.SetMaxDynamicTableSizeLimit(1 << 24)
	e.WriteField(HeaderField{Name: ":method", Value: "GET"})
	if got := hex.EncodeToString(buf.Bytes()); got != "3fe1ff0382" {
		t.Fatalf("after raising the limit = %s, want 3fe1ff0382 "+
			"(the bound the peer set, now unclamped)", got)
	}
}
