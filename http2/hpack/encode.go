// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package hpack

import (
	"io"
)

const (
	uint32Max              = ^uint32(0)
	initialHeaderTableSize = 4096
)

// IndexingPolicy controls how the HPACK encoder decides whether to index headers.
type IndexingPolicy int

const (
	// IndexingDefault uses the default Go behavior (index if fits in table)
	IndexingDefault IndexingPolicy = iota
	// IndexingChrome emulates Chrome's indexing behavior
	IndexingChrome
	// IndexingNever never indexes any headers (always use literal)
	IndexingNever
	// IndexingAlways always indexes headers (if they fit)
	IndexingAlways
)

// IndexingFunc is a custom function to decide whether to index a header.
// Return true to index, false to not index.
type IndexingFunc func(f HeaderField) bool

type Encoder struct {
	dynTab dynamicTable
	// minSize is the minimum table size set by
	// SetMaxDynamicTableSize after the previous Header Table Size
	// Update.
	minSize uint32
	// maxSizeLimit is the maximum table size this encoder
	// supports. This will protect the encoder from too large
	// size.
	maxSizeLimit uint32
	// tableSizeUpdate indicates whether "Header Table Size
	// Update" is required.
	tableSizeUpdate bool
	w               io.Writer
	buf             []byte

	// === Fingerprinting Fields ===

	// IndexPolicy controls the indexing behavior for fingerprinting.
	// Default is IndexingDefault which uses standard Go behavior.
	IndexPolicy IndexingPolicy

	// CustomIndexingFunc allows custom logic for deciding whether to index.
	// If set, this takes precedence over IndexPolicy.
	CustomIndexingFunc IndexingFunc

	// NeverIndexHeaders is a set of header names that should never be indexed.
	// This is useful for fingerprinting as Chrome never indexes certain headers.
	NeverIndexHeaders map[string]bool

	// AlwaysIndexHeaders is a set of header names that should always be indexed.
	AlwaysIndexHeaders map[string]bool
}

// NewEncoder returns a new Encoder which performs HPACK encoding. An
// encoded data is written to w.
func NewEncoder(w io.Writer) *Encoder {
	e := &Encoder{
		minSize:         uint32Max,
		maxSizeLimit:    initialHeaderTableSize,
		tableSizeUpdate: false,
		w:               w,
	}
	e.dynTab.table.init()
	e.dynTab.setMaxSize(initialHeaderTableSize)
	return e
}

// WriteField encodes f into a single Write to e's underlying Writer.
// This function may also produce bytes for "Header Table Size Update"
// if necessary. If produced, it is done before encoding f.
func (e *Encoder) WriteField(f HeaderField) error {
	e.buf = e.buf[:0]

	if e.tableSizeUpdate {
		e.tableSizeUpdate = false
		if e.minSize < e.dynTab.maxSize {
			e.buf = appendTableSize(e.buf, e.minSize)
		}
		e.minSize = uint32Max
		e.buf = appendTableSize(e.buf, e.dynTab.maxSize)
	}

	idx, nameValueMatch := e.searchTable(f)
	if nameValueMatch {
		e.buf = appendIndexed(e.buf, idx)
	} else {
		indexing := e.shouldIndex(f)
		if indexing {
			e.dynTab.add(f)
		}

		if idx == 0 {
			e.buf = appendNewName(e.buf, f, indexing)
		} else {
			e.buf = appendIndexedName(e.buf, f, idx, indexing)
		}
	}
	n, err := e.w.Write(e.buf)
	if err == nil && n != len(e.buf) {
		err = io.ErrShortWrite
	}
	return err
}

// searchTable searches f in both stable and dynamic header tables.
// The static header table is searched first. Only when there is no
// exact match for both name and value, the dynamic header table is
// then searched. If there is no match, i is 0. If both name and value
// match, i is the matched index and nameValueMatch becomes true. If
// only name matches, i points to that index and nameValueMatch
// becomes false.
func (e *Encoder) searchTable(f HeaderField) (i uint64, nameValueMatch bool) {
	i, nameValueMatch = staticTable.search(f)
	if nameValueMatch {
		return i, true
	}

	j, nameValueMatch := e.dynTab.table.search(f)
	if nameValueMatch || (i == 0 && j != 0) {
		return j + uint64(staticTable.len()), nameValueMatch
	}

	return i, false
}

// SetMaxDynamicTableSize changes the dynamic header table size to v.
// The actual size is bounded by the value passed to
// SetMaxDynamicTableSizeLimit.
func (e *Encoder) SetMaxDynamicTableSize(v uint32) {
	if v > e.maxSizeLimit {
		v = e.maxSizeLimit
	}
	if v < e.minSize {
		e.minSize = v
	}
	e.tableSizeUpdate = true
	e.dynTab.setMaxSize(v)
}

// MaxDynamicTableSize returns the current dynamic header table size.
func (e *Encoder) MaxDynamicTableSize() (v uint32) {
	return e.dynTab.maxSize
}

// SetMaxDynamicTableSizeLimit changes the maximum value that can be
// specified in SetMaxDynamicTableSize to v. By default, it is set to
// 4096, which is the same size of the default dynamic header table
// size described in HPACK specification. If the current maximum
// dynamic header table size is strictly greater than v, "Header Table
// Size Update" will be done in the next WriteField call and the
// maximum dynamic header table size is truncated to v.
func (e *Encoder) SetMaxDynamicTableSizeLimit(v uint32) {
	e.maxSizeLimit = v
	if e.dynTab.maxSize > v {
		e.tableSizeUpdate = true
		e.dynTab.setMaxSize(v)
	}
}

// shouldIndex reports whether f should be indexed.
func (e *Encoder) shouldIndex(f HeaderField) bool {
	// Never index sensitive headers
	if f.Sensitive {
		return false
	}

	// Check if header fits in table
	if f.Size() > e.dynTab.maxSize {
		return false
	}

	// Custom indexing function takes precedence
	if e.CustomIndexingFunc != nil {
		return e.CustomIndexingFunc(f)
	}

	// Check never/always index lists
	if e.NeverIndexHeaders != nil && e.NeverIndexHeaders[f.Name] {
		return false
	}
	if e.AlwaysIndexHeaders != nil && e.AlwaysIndexHeaders[f.Name] {
		return true
	}

	// Apply indexing policy
	switch e.IndexPolicy {
	case IndexingNever:
		return false
	case IndexingAlways:
		return true
	case IndexingChrome:
		return e.chromeIndexingBehavior(f)
	default:
		// IndexingDefault - original behavior
		return true
	}
}

// chromeIndexingBehavior is a port of Chromium's HPACK encoder policy, from
// quiche/http2/hpack/hpack_encoder.cc:
//
//	bool DefaultPolicy(absl::string_view name, absl::string_view /* value */) {
//	  if (name.empty()) return false;
//	  if (name[0] == kPseudoHeaderPrefix) {
//	    return name == ":authority";
//	  }
//	  return true;
//	}
//
// Every regular header is indexed, including cookie and authorization, and
// including large values: Chrome inserts a 480-character _abck cookie crumb and
// references it with one byte on the next request. There is no size threshold
// and no per-name exclusion list; adding either diverges from Chrome on every
// request after the first, which is the opposite of what such a list looks like
// it is doing.
//
// The one pseudo-header Chrome indexes is :authority. The other pseudo-headers
// it sends are full static-table value matches (:method GET/POST, :scheme
// https, :path /), so their indexing decision is never reached and excluding
// them looks harmless right up until :authority, which is not a value match and
// therefore has to go out as a literal.
func (e *Encoder) chromeIndexingBehavior(f HeaderField) bool {
	name := f.Name
	if name == "" {
		return false
	}
	if name[0] == ':' {
		return name == ":authority"
	}
	return true
}

// SetIndexingPolicy sets the indexing policy for this encoder.
func (e *Encoder) SetIndexingPolicy(policy IndexingPolicy) {
	e.IndexPolicy = policy
}

// SetCustomIndexingFunc sets a custom function for indexing decisions.
func (e *Encoder) SetCustomIndexingFunc(fn IndexingFunc) {
	e.CustomIndexingFunc = fn
}

// SetNeverIndexHeaders sets headers that should never be indexed.
func (e *Encoder) SetNeverIndexHeaders(headers []string) {
	e.NeverIndexHeaders = make(map[string]bool)
	for _, h := range headers {
		e.NeverIndexHeaders[h] = true
	}
}

// SetAlwaysIndexHeaders sets headers that should always be indexed.
func (e *Encoder) SetAlwaysIndexHeaders(headers []string) {
	e.AlwaysIndexHeaders = make(map[string]bool)
	for _, h := range headers {
		e.AlwaysIndexHeaders[h] = true
	}
}

// appendIndexed appends index i, as encoded in "Indexed Header Field"
// representation, to dst and returns the extended buffer.
func appendIndexed(dst []byte, i uint64) []byte {
	first := len(dst)
	dst = appendVarInt(dst, 7, i)
	dst[first] |= 0x80
	return dst
}

// appendNewName appends f, as encoded in one of "Literal Header field
// - New Name" representation variants, to dst and returns the
// extended buffer.
//
// If f.Sensitive is true, "Never Indexed" representation is used. If
// f.Sensitive is false and indexing is true, "Incremental Indexing"
// representation is used.
func appendNewName(dst []byte, f HeaderField, indexing bool) []byte {
	dst = append(dst, encodeTypeByte(indexing, f.Sensitive))
	dst = appendHpackString(dst, f.Name)
	return appendHpackString(dst, f.Value)
}

// appendIndexedName appends f and index i referring indexed name
// entry, as encoded in one of "Literal Header field - Indexed Name"
// representation variants, to dst and returns the extended buffer.
//
// If f.Sensitive is true, "Never Indexed" representation is used. If
// f.Sensitive is false and indexing is true, "Incremental Indexing"
// representation is used.
func appendIndexedName(dst []byte, f HeaderField, i uint64, indexing bool) []byte {
	first := len(dst)
	var n byte
	if indexing {
		n = 6
	} else {
		n = 4
	}
	dst = appendVarInt(dst, n, i)
	dst[first] |= encodeTypeByte(indexing, f.Sensitive)
	return appendHpackString(dst, f.Value)
}

// appendTableSize appends v, as encoded in "Header Table Size Update"
// representation, to dst and returns the extended buffer.
func appendTableSize(dst []byte, v uint32) []byte {
	first := len(dst)
	dst = appendVarInt(dst, 5, uint64(v))
	dst[first] |= 0x20
	return dst
}

// appendVarInt appends i, as encoded in variable integer form using n
// bit prefix, to dst and returns the extended buffer.
//
// See
// https://httpwg.org/specs/rfc7541.html#integer.representation
func appendVarInt(dst []byte, n byte, i uint64) []byte {
	k := uint64((1 << n) - 1)
	if i < k {
		return append(dst, byte(i))
	}
	dst = append(dst, byte(k))
	i -= k
	for ; i >= 128; i >>= 7 {
		dst = append(dst, byte(0x80|(i&0x7f)))
	}
	return append(dst, byte(i))
}

// appendHpackString appends s, as encoded in "String Literal"
// representation, to dst and returns the extended buffer.
//
// s will be encoded in Huffman codes only when it produces strictly
// shorter byte string.
func appendHpackString(dst []byte, s string) []byte {
	huffmanLength := HuffmanEncodeLength(s)
	if huffmanLength < uint64(len(s)) {
		first := len(dst)
		dst = appendVarInt(dst, 7, huffmanLength)
		dst = AppendHuffmanString(dst, s)
		dst[first] |= 0x80
	} else {
		dst = appendVarInt(dst, 7, uint64(len(s)))
		dst = append(dst, s...)
	}
	return dst
}

// encodeTypeByte returns type byte. If sensitive is true, type byte
// for "Never Indexed" representation is returned. If sensitive is
// false and indexing is true, type byte for "Incremental Indexing"
// representation is returned. Otherwise, type byte for "Without
// Indexing" is returned.
func encodeTypeByte(indexing, sensitive bool) byte {
	if sensitive {
		return 0x10
	}
	if indexing {
		return 0x40
	}
	return 0
}
