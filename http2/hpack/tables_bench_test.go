// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package hpack

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"
)

// churnFields returns n fields whose values never repeat, so every field is
// encoded as a literal with incremental indexing and inserts into the dynamic
// table.
func churnFields(n, start int) []HeaderField {
	fields := make([]HeaderField, n)
	filler := strings.Repeat("a", 48)
	for i := range fields {
		fields[i] = HeaderField{
			Name:  "x-header-" + strconv.Itoa(i%16),
			Value: "value-" + strconv.Itoa(start+i) + "-" + filler,
		}
	}
	return fields
}

// BenchmarkDecoderTableChurn decodes header blocks whose fields all carry
// never-repeating values into a Chrome-sized dynamic table, so steady-state
// decoding inserts and evicts an entry per field.
func BenchmarkDecoderTableChurn(b *testing.B) {
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	for _, f := range churnFields(512, 0) {
		if err := e.WriteField(f); err != nil {
			b.Fatal(err)
		}
	}
	block := buf.Bytes()
	d := NewDecoder(65536, func(HeaderField) {})
	b.ReportAllocs()
	for b.Loop() {
		if _, err := d.Write(block); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEncoderTableChurn encodes never-repeating fields into a
// Chrome-sized dynamic table, so steady-state encoding inserts and evicts an
// entry per field.
func BenchmarkEncoderTableChurn(b *testing.B) {
	e := NewEncoder(io.Discard)
	e.SetMaxDynamicTableSizeLimit(65536)
	e.SetMaxDynamicTableSize(65536)
	// Cycling through 8 pregenerated blocks keeps value generation out of the
	// loop; by the time a block comes around again its entries have long been
	// evicted, so no field ever gets an index match.
	blocks := make([][]HeaderField, 8)
	for i := range blocks {
		blocks[i] = churnFields(512, i*512)
	}
	i := 0
	b.ReportAllocs()
	for b.Loop() {
		for _, f := range blocks[i%len(blocks)] {
			if err := e.WriteField(f); err != nil {
				b.Fatal(err)
			}
		}
		i++
	}
}
