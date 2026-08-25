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
	// settingsSizeBound is the last SETTINGS_HEADER_TABLE_SIZE the peer
	// advertised, before any clamping. It records what the peer SAID, which is
	// not the same as what we chose to honour: comparing against the table
	// size instead looks equivalent and is, right up until the limit is
	// clamping, at which point the table holds the clamped value while this
	// holds the peer's. Get that wrong and a peer that advertises a clamped
	// value twice gets one update from a browser and none from us.
	settingsSizeBound uint32
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

	// Representations pins the HPACK representation for individual header
	// names, overriding IndexPolicy, CustomIndexingFunc and both lists below.
	//
	// It is an OVERRIDE LAYER and is expected to be empty for a browser
	// profile, whose base policy is already correct. It exists for mirroring a
	// client whose representation choices differ from that policy on specific
	// names, and should carry only those deltas. Populating it from raw
	// observation makes every profile restate its own policy and go stale the
	// moment the policy improves.
	//
	// Representation is a per-NAME rule in every stack examined, not an
	// arbitrary per-header choice, which is why a name-keyed map is the right
	// shape. Both curl and Chrome send :path as a literal without indexing
	// because paths vary and would pollute the table; curl never-indexes
	// authorization while Chrome indexes it like anything else.
	Representations map[string]Representation

	// NeverIndexHeaders is a set of header names that should never be indexed.
	// This is useful for fingerprinting as Chrome never indexes certain headers.
	//
	// Equivalent to a Representations entry of RepresentationNever, kept
	// because it predates the map.
	NeverIndexHeaders map[string]bool

	// AlwaysIndexHeaders is a set of header names that should always be indexed.
	AlwaysIndexHeaders map[string]bool
}

// NewEncoder returns a new Encoder which performs HPACK encoding. An
// encoded data is written to w.
func NewEncoder(w io.Writer) *Encoder {
	e := &Encoder{
		minSize:           uint32Max,
		maxSizeLimit:      initialHeaderTableSize,
		settingsSizeBound: initialHeaderTableSize,
		tableSizeUpdate:   false,
		w:                 w,
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
	suppressExactMatch := false

	if e.tableSizeUpdate {
		e.tableSizeUpdate = false
		if e.minSize < e.dynTab.maxSize {
			e.buf = appendTableSize(e.buf, e.minSize)
		}
		e.minSize = uint32Max
		e.buf = appendTableSize(e.buf, e.dynTab.maxSize)
	}

	// A pinned representation is resolved before anything else looks at the
	// field, because two of the three choices have to suppress the exact-match
	// lookup below as well as the indexing decision.
	forceIncremental := false
	if rep, ok := e.Representations[f.Name]; ok {
		switch rep {
		case RepresentationNever:
			f.Sensitive = true
		case RepresentationWithout:
			// 6.2.2 is a literal, so an indexed reference is not allowed even
			// when the table holds an identical field. searchTable only skips
			// its name-and-value lookup for sensitive fields, so this one has
			// to be suppressed explicitly.
			suppressExactMatch = true
		case RepresentationIncremental:
			forceIncremental = true
		}
	}

	// The never-index list has to be resolved into f.Sensitive here, before
	// anything else looks at the field. Sensitive is what the rest of the
	// encoder keys off: encodeTypeByte turns it into the 0x10 prefix (RFC 7541
	// 6.2.3), shouldIndex refuses to add the field to the dynamic table, and
	// headerFieldTable.search skips the name-and-value lookup so the field can
	// never go out as an indexed reference. A name-only index is still used,
	// which 6.2.3 allows.
	//
	// Setting it only inside shouldIndex, as before, reached none of that: the
	// field went out as 0x00, "without indexing", which is a different
	// instruction to the peer than the method name promises.
	//
	// This deliberately outranks CustomIndexingFunc and AlwaysIndexHeaders.
	// The settings contradict each other, and an explicit "never index this"
	// is the one carrying a security meaning, so it wins.
	if !f.Sensitive && e.NeverIndexHeaders != nil && e.NeverIndexHeaders[f.Name] {
		f.Sensitive = true
	}

	idx, nameValueMatch := e.searchTable(f)
	if nameValueMatch && !suppressExactMatch {
		e.buf = appendIndexed(e.buf, idx)
	} else {
		indexing := forceIncremental || e.shouldIndex(f)
		if suppressExactMatch {
			indexing = false // 6.2.2 does not add to the dynamic table
		}
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
	// quiche's HpackEncoder::ApplyHeaderTableSizeSetting, step for step:
	//
	//	if (size_setting == header_table_.settings_size_bound() &&
	//	    size_setting <= table_size_upper_bound_) {
	//	  return;
	//	}
	//	if (size_setting < header_table_.settings_size_bound()) {
	//	  min_table_size_setting_received_ =
	//	      std::min(size_setting, min_table_size_setting_received_);
	//	}
	//	header_table_.SetSettingsHeaderTableSize(size_setting);
	//	if (size_setting > table_size_upper_bound_) {
	//	  header_table_.SetMaxSize(table_size_upper_bound_);
	//	}
	//	should_emit_table_size_ = true;

	// Nothing changed and nothing is being clamped, so there is nothing to
	// tell the peer. Both halves of the condition matter: the second is what
	// leaves a repeat of a clamped value still able to re-arm.
	if v == e.settingsSizeBound && v <= e.maxSizeLimit {
		return
	}

	// The minimum is lowered against the BOUND, not against the running
	// minimum. Guarding on the running minimum makes two SETTINGS frames
	// carrying increasing values, both above the current bound, emit two
	// updates where a browser emits one.
	if v < e.settingsSizeBound && v < e.minSize {
		e.minSize = v
	}

	// Before any clamping: this records the peer's value.
	e.settingsSizeBound = v

	e.tableSizeUpdate = true
	if v > e.maxSizeLimit {
		v = e.maxSizeLimit
	}
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
	// Recompute the target rather than only shrinking. Raising the limit past
	// a bound that was previously being clamped has to move the table up as
	// well, or the encoder keeps using a smaller table than it has told the
	// peer about.
	target := e.settingsSizeBound
	if target > v {
		target = v
	}
	if e.dynTab.maxSize != target {
		e.tableSizeUpdate = true
		e.dynTab.setMaxSize(target)
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

// Representation names the HPACK representation an encoder must use for a
// header field, using RFC 7541's own vocabulary.
type Representation uint8

const (
	// RepresentationDefault leaves the choice to the indexing policy.
	RepresentationDefault Representation = iota
	// RepresentationIncremental is "Literal Header Field with Incremental
	// Indexing" (6.2.1), prefix 0x40. The field is added to the dynamic table,
	// so a later identical field can be sent as an indexed reference.
	RepresentationIncremental
	// RepresentationWithout is "Literal Header Field without Indexing"
	// (6.2.2), prefix 0x00. Never added to the table, and never sent as an
	// indexed reference even when the table already holds an identical field.
	RepresentationWithout
	// RepresentationNever is "Literal Header Field Never Indexed" (6.2.3),
	// prefix 0x10. As above, plus an instruction to intermediaries not to
	// index it either.
	RepresentationNever
)

// ParseRepresentation maps a configuration string to a Representation.
// "default" and the empty string both mean "leave it to the policy".
func ParseRepresentation(s string) (Representation, bool) {
	switch s {
	case "", "default":
		return RepresentationDefault, true
	case "incremental", "indexed":
		return RepresentationIncremental, true
	case "without", "without_indexing":
		return RepresentationWithout, true
	case "never", "never_indexed":
		return RepresentationNever, true
	}
	return RepresentationDefault, false
}

// SetHeaderRepresentations pins the representation for individual header names.
func (e *Encoder) SetHeaderRepresentations(m map[string]Representation) {
	if len(m) == 0 {
		e.Representations = nil
		return
	}
	e.Representations = make(map[string]Representation, len(m))
	for k, v := range m {
		e.Representations[k] = v
	}
}

// SetCustomIndexingFunc sets a custom function for indexing decisions.
func (e *Encoder) SetCustomIndexingFunc(fn IndexingFunc) {
	e.CustomIndexingFunc = fn
}

// SetNeverIndexHeaders sets headers that must be emitted with the "Literal
// Header Field Never Indexed" representation (RFC 7541 6.2.3), the 0x10
// prefix. Such a field is never added to the dynamic table and is never sent
// as an indexed reference, even when an identical name and value pair is
// already in the static or dynamic table.
//
// Most browsers set nothing here. Chrome in particular indexes cookie and
// authorization like any other header, so a Chrome-shaped profile wants this
// list empty rather than populated "for safety".
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
