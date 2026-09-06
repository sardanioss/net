// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package httpcommon

import (
	"hash/maphash"
	"strings"
)

// headerPlanShapes is how many distinct header shapes one cache retains, and
// how many recently missed shape identities it remembers for the admission
// rule below. A connection reused across requests sees a handful of shapes:
// one header-name set per credential flavor the caller rotates through, times
// small per-endpoint and per-method extras such as a content-type on writes
// or a transaction id on some calls. Eight covers that with room to spare and
// keeps the identity scan a walk over a few integers.
const headerPlanShapes = 8

// HeaderPlanCache caches, across requests, the resolution metadata that
// planHeaderOrder derives from a header order list and a header map: which
// stored key each order name resolves to, the ASCII-lowered wire name of
// every key, and how many order slots each key holds.
//
// The order list may be freshly permuted on every request. The cache
// therefore keys on what a permutation preserves, the multiset of order names
// and the set of map keys, and caches nothing that depends on the sequence.
// Header values are read from the request's own map on every call and are
// never cached.
//
// A hash match is never trusted. Replaying a cached shape re-checks every
// order name and every map key against it and falls back to the ordinary
// build on any mismatch, so a plan served from the cache is always identical
// to what planHeaderOrder would have produced for the same inputs.
//
// A shape is only admitted once its identity has missed twice, so a
// connection that encodes a single request, or a caller whose shape changes
// on every request, pays the identity hash and nothing more.
//
// A HeaderPlanCache is not safe for concurrent use. The http2 transport owns
// one per ClientConn and only calls it while holding the connection's write
// lock. A returned plan aliases the cache's scratch storage and the request's
// value slices; it is valid until the next call on the same cache.
type HeaderPlanCache struct {
	seed    maphash.Seed
	shapes  []*planShape   // most recently used first
	misses  []planIdentity // identities that missed once, oldest first
	hits    int            // verified replays served; tests assert the path runs
	scratch planScratch
}

// NewHeaderPlanCache returns an empty cache ready for use by one connection.
// The zero value also works: plan seeds itself on first use.
func NewHeaderPlanCache() *HeaderPlanCache {
	return &HeaderPlanCache{seed: maphash.MakeSeed()}
}

// planIdentity summarizes the cacheable identity of one (order, header map)
// pair: an order-independent hash of the order-name multiset, one of the map
// key set, and both lengths. Two calls with equal identities almost certainly
// share a shape, but only replay's full verification decides.
type planIdentity struct {
	orderHash uint64
	keyHash   uint64
	orderLen  int
	keyLen    int
}

// nameResolution is what one distinct order name resolved to when the shape
// was built. entry follows the planHeaderOrder convention: the key index + 1,
// 0 for a name the map does not hold, and -1 for a content-length slot. id is
// this name's index into the per-replay occurrence counts, and count is how
// many order slots the name held in the cached multiset.
type nameResolution struct {
	entry int32
	id    int32
	count int32
}

// cachedKey is the per-map-key half of a shape: the exact key, its
// ASCII-lowered wire name, and how many order slots resolved to it. A key
// with no slots trails the plan in map iteration order.
type cachedKey struct {
	key      string
	wireName string
	slots    int32
}

// planShape is one cached shape: the resolution of every distinct order name
// and the metadata of every map key, for one (order multiset, key set) pair.
// The stored names and keys are cloned so the shape never pins a caller's
// backing buffers.
type planShape struct {
	id       planIdentity
	names    map[string]nameResolution
	keys     map[string]int32 // exact map key -> index into keyInfo
	keyInfo  []cachedKey
	numNames int
}

// planScratch is the reusable working storage: the build path's entry slice,
// index map and resolution slice, the plan both paths emit into, and replay's
// per-request value, cursor and count slices.
type planScratch struct {
	entries  []headerEntry
	index    map[string]indexEntry
	resolved []int32
	plan     []plannedHeader
	vals     [][]string
	trail    []int32
	counts   []int32
	cursors  []int32
}

// plan returns the emission plan for order against h, exactly as
// planHeaderOrder(order, h) would, serving the resolution metadata from the
// cache when a verified shape matches and rebuilding it otherwise.
func (c *HeaderPlanCache) plan(order []string, h map[string][]string) []plannedHeader {
	if c.seed == (maphash.Seed{}) {
		// A zero-value cache has no seed yet; hashing with one panics.
		c.seed = maphash.MakeSeed()
	}
	id := c.identity(order, h)
	if s := c.find(id); s != nil {
		if plan, ok := c.replay(s, order, h); ok {
			c.hits++
			return plan
		}
		// The identity matched but the shape did not: a hash collision, by
		// construction the only way here. Drop the shape; the rebuild below
		// re-admits whichever identity keeps arriving.
		c.remove(s)
	}
	plan := planHeaderOrderInto(order, h, &c.scratch)
	if c.admit(id) {
		c.insert(c.buildShape(order, id))
	}
	return plan
}

// identity hashes the order-name multiset and the map key set with the
// cache's seed. Addition makes both hashes order-independent while keeping
// duplicate names distinct from their absence.
func (c *HeaderPlanCache) identity(order []string, h map[string][]string) planIdentity {
	var oh, kh uint64
	for _, k := range order {
		oh += maphash.String(c.seed, k)
	}
	for k := range h {
		kh += maphash.String(c.seed, k)
	}
	return planIdentity{orderHash: oh, keyHash: kh, orderLen: len(order), keyLen: len(h)}
}

// find returns the cached shape with the given identity, moving it to the
// front of the use order, or nil.
func (c *HeaderPlanCache) find(id planIdentity) *planShape {
	for i, s := range c.shapes {
		if s.id == id {
			if i > 0 {
				copy(c.shapes[1:i+1], c.shapes[:i])
				c.shapes[0] = s
			}
			return s
		}
	}
	return nil
}

// remove drops a shape from the cache.
func (c *HeaderPlanCache) remove(s *planShape) {
	for i, cs := range c.shapes {
		if cs == s {
			c.shapes = append(c.shapes[:i], c.shapes[i+1:]...)
			return
		}
	}
}

// insert adds a freshly built shape at the front, evicting the least recently
// used one when the cache is full.
func (c *HeaderPlanCache) insert(s *planShape) {
	if len(c.shapes) >= headerPlanShapes {
		c.shapes = c.shapes[:headerPlanShapes-1]
	}
	c.shapes = append(c.shapes, nil)
	copy(c.shapes[1:], c.shapes)
	c.shapes[0] = s
}

// admit reports whether an identity that just missed should be cached now.
// The first miss only records the identity; the second one within the
// remembered window builds the shape. A shape seen once, the whole life of a
// one-request connection included, never pays for a build.
func (c *HeaderPlanCache) admit(id planIdentity) bool {
	for i, m := range c.misses {
		if m == id {
			c.misses = append(c.misses[:i], c.misses[i+1:]...)
			return true
		}
	}
	if len(c.misses) >= headerPlanShapes {
		copy(c.misses, c.misses[1:])
		c.misses = c.misses[:headerPlanShapes-1]
	}
	c.misses = append(c.misses, id)
	return false
}

// buildShape captures the shape of the build that just ran, reading the entry
// slice and per-slot resolutions planHeaderOrderInto left in the scratch.
// Stored names and keys are cloned so the shape holds no reference into the
// request that built it.
func (c *HeaderPlanCache) buildShape(order []string, id planIdentity) *planShape {
	entries := c.scratch.entries
	resolved := c.scratch.resolved
	s := &planShape{
		id:      id,
		names:   make(map[string]nameResolution, len(order)),
		keys:    make(map[string]int32, len(entries)),
		keyInfo: make([]cachedKey, len(entries)),
	}
	for i := range entries {
		e := &entries[i]
		key := strings.Clone(e.key)
		wireName := key
		if e.lower != e.key {
			wireName = strings.Clone(e.lower)
		}
		s.keys[key] = int32(i)
		s.keyInfo[i] = cachedKey{key: key, wireName: wireName, slots: e.slots}
	}
	for oi, k := range order {
		if nr, ok := s.names[k]; ok {
			nr.count++
			s.names[k] = nr
			continue
		}
		s.names[strings.Clone(k)] = nameResolution{
			entry: resolved[oi],
			id:    int32(len(s.names)),
			count: 1,
		}
	}
	s.numNames = len(s.names)
	return s
}

// replay builds the plan for this request from a cached shape, verifying the
// shape against the inputs as it goes. It reports false on any divergence,
// and a false return means the caller must rebuild; nothing was cached from
// the failed attempt.
//
// The verification is total, not a spot check. Every map key must be present
// in the shape and the lengths already matched through the identity, so the
// key sets are equal. Every order name must be known to the shape, no name
// may occur more often than the cached multiset holds, and the total lengths
// match, which together force the multisets to be equal. Resolution, wire
// names and slot counts are pure functions of the key set and the name
// multiset, so on a verified replay they are exactly what a rebuild would
// have computed.
func (c *HeaderPlanCache) replay(s *planShape, order []string, h map[string][]string) ([]plannedHeader, bool) {
	sc := &c.scratch
	if cap(sc.vals) < len(s.keyInfo) {
		sc.vals = make([][]string, len(s.keyInfo))
	} else {
		sc.vals = sc.vals[:len(s.keyInfo)]
		clear(sc.vals)
	}
	vals := sc.vals
	trail := sc.trail[:0]
	for k, vv := range h {
		id, ok := s.keys[k]
		if !ok {
			sc.trail = trail
			return nil, false
		}
		vals[id] = vv
		if s.keyInfo[id].slots == 0 {
			trail = append(trail, id)
		}
	}
	sc.trail = trail

	if cap(sc.counts) < s.numNames {
		sc.counts = make([]int32, s.numNames)
	} else {
		sc.counts = sc.counts[:s.numNames]
		clear(sc.counts)
	}
	counts := sc.counts
	if cap(sc.cursors) < len(s.keyInfo) {
		sc.cursors = make([]int32, len(s.keyInfo))
	} else {
		sc.cursors = sc.cursors[:len(s.keyInfo)]
		clear(sc.cursors)
	}
	cursors := sc.cursors

	plan := sc.plan[:0]
	for _, k := range order {
		nr, ok := s.names[k]
		if !ok || counts[nr.id] == nr.count {
			sc.plan = plan
			return nil, false
		}
		counts[nr.id]++
		switch nr.entry {
		case -1:
			plan = append(plan, plannedHeader{contentLength: true})
		case 0:
		default:
			id := nr.entry - 1
			ki := &s.keyInfo[id]
			cur := cursors[id]
			cursors[id]++
			if vv := valuesForSlot(vals[id], int(ki.slots), int(cur)); len(vv) > 0 {
				plan = append(plan, plannedHeader{key: ki.key, wireName: ki.wireName, values: vv})
			}
		}
	}
	for _, id := range trail {
		ki := &s.keyInfo[id]
		plan = append(plan, plannedHeader{key: ki.key, wireName: ki.wireName, values: vals[id]})
	}
	sc.plan = plan
	return plan, true
}
