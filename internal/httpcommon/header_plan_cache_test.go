package httpcommon

import (
	"fmt"
	"math/rand"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"testing"
)

func mustParseURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// planHeaderOrderRef is the pre-cache planHeaderOrder, kept verbatim as the
// behavioural reference for the differential tests below. It additionally
// reports how many plan entries came from order slots, because everything
// after them trails in map iteration order and has to be compared as a set.
func planHeaderOrderRef(order []string, h map[string][]string) (plan []plannedHeader, orderedLen int) {
	type headerEntry struct {
		key    string
		lower  string
		values []string
		slots  int32
		cursor int32
	}
	type indexEntry struct {
		exact int32
		fold  int32
	}
	entries := make([]headerEntry, 0, len(h))
	index := make(map[string]indexEntry, 2*len(h))
	for hk, vv := range h {
		lk := foldKey(hk)
		i := int32(len(entries) + 1)
		entries = append(entries, headerEntry{key: hk, lower: lk, values: vv})
		if hk == lk {
			e := index[hk]
			e.exact = i
			if e.fold == 0 || hk < entries[e.fold-1].key {
				e.fold = i
			}
			index[hk] = e
		} else {
			e := index[hk]
			e.exact = i
			index[hk] = e
			fe := index[lk]
			if fe.fold == 0 || hk < entries[fe.fold-1].key {
				fe.fold = i
				index[lk] = fe
			}
		}
	}

	resolved := make([]int32, len(order))
	for oi, k := range order {
		if asciiEqualFold(k, "content-length") {
			resolved[oi] = -1
			continue
		}
		e, ok := index[k]
		if !ok || e.exact == 0 {
			if fk := foldKey(k); fk != k {
				e = index[fk]
			}
			if e.fold == 0 {
				continue
			}
			resolved[oi] = e.fold
			entries[e.fold-1].slots++
			continue
		}
		resolved[oi] = e.exact
		entries[e.exact-1].slots++
	}

	trailing := 0
	for i := range entries {
		if entries[i].slots == 0 {
			trailing++
		}
	}

	plan = make([]plannedHeader, 0, len(order)+trailing)
	for _, r := range resolved {
		switch r {
		case -1:
			plan = append(plan, plannedHeader{contentLength: true})
		case 0:
		default:
			e := &entries[r-1]
			i := e.cursor
			e.cursor++
			if vv := valuesForSlot(e.values, int(e.slots), int(i)); len(vv) > 0 {
				plan = append(plan, plannedHeader{key: e.key, wireName: e.lower, values: vv})
			}
		}
	}
	orderedLen = len(plan)
	for i := range entries {
		if e := &entries[i]; e.slots == 0 {
			plan = append(plan, plannedHeader{key: e.key, wireName: e.lower, values: e.values})
		}
	}
	return plan, orderedLen
}

// planString renders one planned emission for comparison and failure output.
func planString(p plannedHeader) string {
	if p.contentLength {
		return "<content-length>"
	}
	return fmt.Sprintf("%s|%s|%q", p.key, p.wireName, p.values)
}

// comparePlans checks got against the reference plan: the ordered prefix must
// match exactly, and the trailing entries, which both implementations emit in
// map iteration order, must match as a set.
func comparePlans(t *testing.T, label string, got, want []plannedHeader, orderedLen int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: plan length = %d, want %d\ngot:  %v\nwant: %v",
			label, len(got), len(want), render(got), render(want))
	}
	for i := 0; i < orderedLen; i++ {
		if planString(got[i]) != planString(want[i]) {
			t.Fatalf("%s: plan[%d] = %s, want %s\ngot:  %v\nwant: %v",
				label, i, planString(got[i]), planString(want[i]), render(got), render(want))
		}
	}
	gotTail, wantTail := render(got[orderedLen:]), render(want[orderedLen:])
	sort.Strings(gotTail)
	sort.Strings(wantTail)
	if !slices.Equal(gotTail, wantTail) {
		t.Fatalf("%s: trailing entries = %v, want %v", label, gotTail, wantTail)
	}
}

func render(plan []plannedHeader) []string {
	out := make([]string, len(plan))
	for i, p := range plan {
		out[i] = planString(p)
	}
	return out
}

// clonePlan deep-copies a plan's own storage so a cached plan survives the
// next call on the same cache for comparison. Value slices still alias the
// header map, which the comparisons want.
func clonePlan(plan []plannedHeader) []plannedHeader {
	return slices.Clone(plan)
}

// shapeCase is one generated (order template, header map template) shape. A
// request instance is a fresh permutation of the order with fresh values.
type shapeCase struct {
	header map[string][]string
	order  []string
}

// genShape builds a randomized shape: header keys in mixed casings, some
// sharing a fold class, values of varying counts including empty, an order
// naming present keys (in either casing), absent names, content-length slots,
// duplicate slots, magic ordering keys and a non-ASCII name.
func genShape(rng *rand.Rand) shapeCase {
	baseNames := []string{
		"accept", "authorization", "cookie", "x-thing", "x-custom-one",
		"referer", "content-type", "x-f\xc3\xbc", "Header-Order:",
		// A real content-length map key never resolves (order slots for the
		// name bypass the map entirely), and a pseudo-looking key is just a
		// key to the planner.
		"Content-Length", ":protocol",
	}
	casings := func(s string) []string {
		out := []string{s}
		b := []byte(s)
		for i := range b {
			if 'a' <= b[i] && b[i] <= 'z' {
				b[i] -= 'a' - 'A'
				out = append(out, string(b))
				break
			}
		}
		return out
	}
	h := make(map[string][]string)
	var present []string
	for _, n := range baseNames {
		if rng.Intn(3) == 0 {
			continue
		}
		for _, c := range casings(n) {
			if rng.Intn(2) == 0 {
				continue
			}
			var vv []string
			for j := 0; j < rng.Intn(4); j++ {
				vv = append(vv, "v"+strconv.Itoa(j))
			}
			h[c] = vv
			present = append(present, c)
		}
	}
	var order []string
	for _, k := range present {
		reps := 1 + rng.Intn(2)
		for j := 0; j < reps; j++ {
			if rng.Intn(2) == 0 {
				order = append(order, foldKey(k))
			} else {
				order = append(order, k)
			}
		}
	}
	for _, absent := range []string{"x-absent", "X-Missing", ""} {
		if rng.Intn(2) == 0 {
			order = append(order, absent)
		}
	}
	if rng.Intn(2) == 0 {
		order = append(order, "content-length")
	}
	if rng.Intn(4) == 0 {
		order = append(order, "Content-Length")
	}
	rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	return shapeCase{header: h, order: order}
}

// instance returns a fresh permutation of the shape's order and mutates one
// value in place, the way a real caller changes authorization and transaction
// ids between requests of the same shape.
func (s shapeCase) instance(rng *rand.Rand, round int) []string {
	order := slices.Clone(s.order)
	rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	for k, vv := range s.header {
		if len(vv) > 0 {
			vv[0] = "r" + strconv.Itoa(round) + k
			break
		}
	}
	return order
}

// TestPlanCacheDifferential drives one cache through interleaved randomized
// shapes, fresh permutations and mutated values per round, comparing every
// plan (cached path and plain path) against the verbatim pre-cache
// implementation, across several seeds. Shapes mutate mid-stream so the
// invalidation path runs too.
func TestPlanCacheDifferential(t *testing.T) {
	for seed := int64(1); seed <= 8; seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			c := NewHeaderPlanCache()
			shapes := make([]shapeCase, 3)
			for i := range shapes {
				shapes[i] = genShape(rng)
			}
			for round := 0; round < 200; round++ {
				si := rng.Intn(len(shapes))
				if rng.Intn(25) == 0 {
					// Mutate the shape in place: the cache must notice and
					// rebuild rather than serve the stale resolution.
					shapes[si] = genShape(rng)
				}
				s := shapes[si]
				order := s.instance(rng, round)
				label := fmt.Sprintf("round %d shape %d", round, si)

				want, orderedLen := planHeaderOrderRef(order, s.header)
				got := clonePlan(c.plan(order, s.header))
				comparePlans(t, label+" cached", got, want, orderedLen)
				plain := planHeaderOrder(order, s.header)
				comparePlans(t, label+" plain", plain, want, orderedLen)
			}
			// The comparisons above prove nothing about replay unless the
			// stream actually hit the cache.
			if c.hits < 50 {
				t.Errorf("cache hits = %d over 200 rounds, want at least 50; the replay path went untested", c.hits)
			}
		})
	}
}

// TestPlanCacheEmptyInputs covers the degenerate corners through both paths.
func TestPlanCacheEmptyInputs(t *testing.T) {
	c := NewHeaderPlanCache()
	cases := []shapeCase{
		{header: nil, order: []string{"accept", "content-length"}},
		{header: map[string][]string{}, order: []string{"x"}},
		{header: map[string][]string{"Accept": {"a"}}, order: nil},
		{header: map[string][]string{"Accept": {"a"}}, order: []string{}},
	}
	for i, s := range cases {
		for round := 0; round < 3; round++ {
			want, orderedLen := planHeaderOrderRef(s.order, s.header)
			got := clonePlan(c.plan(s.order, s.header))
			comparePlans(t, fmt.Sprintf("case %d round %d", i, round), got, want, orderedLen)
		}
	}
}

// prime runs a shape through a cache until it is admitted and returns the
// cached planShape, failing if admission did not happen.
func prime(t *testing.T, c *HeaderPlanCache, order []string, h map[string][]string) *planShape {
	t.Helper()
	c.plan(order, h)
	c.plan(order, h)
	s := c.find(c.identity(order, h))
	if s == nil {
		t.Fatal("shape was not admitted after two calls")
	}
	return s
}

// TestReplayRejectsMismatches drives replay directly against inputs that a
// colliding identity could smuggle past the hash, one divergence per case.
// Every one must be rejected.
func TestReplayRejectsMismatches(t *testing.T) {
	h := map[string][]string{
		"Accept": {"a"},
		"Cookie": {"x=1"},
		"X-A":    {"1"},
	}
	order := []string{"accept", "cookie", "accept"}
	c := NewHeaderPlanCache()
	s := prime(t, c, order, h)

	if _, ok := c.replay(s, order, h); !ok {
		t.Fatal("replay rejected the inputs the shape was built from")
	}
	cases := []struct {
		name  string
		order []string
		h     map[string][]string
	}{
		{"unknown order name", []string{"accept", "cookie", "x-new"}, h},
		{"over-count of a name", []string{"accept", "accept", "accept"}, h},
		{"unknown map key", order, map[string][]string{"Accept": {"a"}, "Cookie": {"x=1"}, "X-B": {"1"}}},
		{"recased map key", order, map[string][]string{"Accept": {"a"}, "Cookie": {"x=1"}, "x-a": {"1"}}},
	}
	for _, tc := range cases {
		if _, ok := c.replay(s, tc.order, tc.h); ok {
			t.Errorf("%s: replay accepted diverging inputs", tc.name)
		}
	}
}

// TestPlanCacheAdmission checks the double-miss rule: one call must not build
// a shape, the second must, and an unrepeated shape must never be admitted.
func TestPlanCacheAdmission(t *testing.T) {
	h := map[string][]string{"Accept": {"a"}}
	order := []string{"accept"}
	c := NewHeaderPlanCache()
	c.plan(order, h)
	if len(c.shapes) != 0 {
		t.Fatalf("after one call: %d shapes cached, want 0", len(c.shapes))
	}
	c.plan(order, h)
	if len(c.shapes) != 1 {
		t.Fatalf("after two calls: %d shapes cached, want 1", len(c.shapes))
	}
	// A parade of one-off shapes must not evict the live one or be admitted.
	for i := 0; i < 3*headerPlanShapes; i++ {
		c.plan([]string{"x-" + strconv.Itoa(i)}, map[string][]string{"X-" + strconv.Itoa(i): {"v"}})
	}
	if len(c.shapes) != 1 {
		t.Fatalf("after one-off parade: %d shapes cached, want 1", len(c.shapes))
	}
}

// TestPlanCacheThroughEncodeHeaders runs the cache through the public entry
// point against the uncached output, over repeated permutations of the
// production-like benchmark shape, so the emitted wire sequence is compared
// end to end, cookie splitting, content-length and user-agent handling
// included.
func TestPlanCacheThroughEncodeHeaders(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	c := NewHeaderPlanCache()
	for round := 0; round < 50; round++ {
		header := map[string][]string{
			"Accept":        {"application/json"},
			"Authorization": {"Bearer tok" + strconv.Itoa(round)},
			"Cookie":        {"a=1; b=2"},
			"X-Thing":       {"v"},
		}
		order := []string{"authorization", "accept", "content-length", "cookie", "x-thing", "x-absent"}
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

		run := func(cache *HeaderPlanCache) []string {
			t.Helper()
			var got []string
			param := testParam(header, order)
			param.PlanCache = cache
			_, err := EncodeHeaders(t.Context(), param, func(name, value string) {
				got = append(got, name+": "+value)
			})
			if err != nil {
				t.Fatalf("EncodeHeaders: %v", err)
			}
			return got
		}
		cached := run(c)
		plain := run(nil)
		if !slices.Equal(cached, plain) {
			t.Fatalf("round %d: cached emissions diverge\ncached: %v\nplain:  %v", round, cached, plain)
		}
	}
}

func testParam(header map[string][]string, order []string) EncodeHeadersParam {
	u := mustParseURL("https://example.com/some/path?x=1")
	return EncodeHeadersParam{
		Request: Request{
			URL:                 u,
			Method:              "POST",
			Host:                "example.com",
			Header:              header,
			ActualContentLength: 42,
		},
		DefaultUserAgent:      "test-agent",
		HeaderOrder:           order,
		DisableCookieSplit:    true,
		PeerMaxHeaderListSize: 262144,
	}
}

// benchShape returns the production-shaped fixture the cache benchmarks
// share: a canonical-cased header map against a lowercase order that also
// names headers the request does not carry.
func benchShape() (map[string][]string, []string) {
	header := map[string][]string{
		"Accept":             {"application/json"},
		"Accept-Encoding":    {"gzip, deflate, br"},
		"Accept-Language":    {"en-US,en;q=0.9"},
		"Authorization":      {"Bearer 0123456789abcdef0123456789abcdef"},
		"Cache-Control":      {"no-cache"},
		"Cookie":             {"a=1; b=2; c=3"},
		"Origin":             {"https://example.com"},
		"Referer":            {"https://example.com/"},
		"User-Agent":         {"bench-agent/1.0"},
		"X-Csrf-Token":       {"abcdef"},
		"X-Custom-One":       {"v1"},
		"X-Custom-Two":       {"v2"},
		"X-Requested-With":   {"XMLHttpRequest"},
		"Sec-Ch-Ua-Platform": {`"Linux"`},
	}
	order := []string{
		"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "upgrade-insecure-requests",
		"authorization", "user-agent", "x-csrf-token", "x-requested-with", "content-type",
		"accept", "origin", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest",
		"referer", "accept-encoding", "accept-language", "cookie",
		"x-custom-one", "x-custom-two", "cache-control", "pragma", "range", "if-none-match",
	}
	return header, order
}

// benchPermutations returns n fixed pre-shuffled copies of order, so a
// benchmark iterates over fresh permutations without paying for the shuffle
// inside the timed loop.
func benchPermutations(order []string, n int) [][]string {
	rng := rand.New(rand.NewSource(7))
	perms := make([][]string, n)
	for i := range perms {
		p := slices.Clone(order)
		rng.Shuffle(len(p), func(a, b int) { p[a], p[b] = p[b], p[a] })
		perms[i] = p
	}
	return perms
}

// The plan build as it runs today, allocating its working storage fresh.
func BenchmarkPlanHeaderOrder(b *testing.B) {
	header, order := benchShape()
	perms := benchPermutations(order, 64)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		planHeaderOrder(perms[i%len(perms)], header)
		i++
	}
}

// The cheap alternative: the same build with pooled working storage and no
// shape caching.
func BenchmarkPlanHeaderOrderScratch(b *testing.B) {
	header, order := benchShape()
	perms := benchPermutations(order, 64)
	var scratch planScratch
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		planHeaderOrderInto(perms[i%len(perms)], header, &scratch)
		i++
	}
}

// The steady state of the full cache: a fresh permutation of one admitted
// shape per call.
func BenchmarkPlanHeaderOrderCacheHit(b *testing.B) {
	header, order := benchShape()
	perms := benchPermutations(order, 64)
	c := NewHeaderPlanCache()
	c.plan(perms[0], header)
	c.plan(perms[1], header)
	c.plan(perms[2], header)
	if c.hits != 1 || len(c.shapes) != 1 {
		b.Fatalf("cache not primed: hits = %d, shapes = %d", c.hits, len(c.shapes))
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.plan(perms[i%len(perms)], header)
		i++
	}
}

// End to end steady state: EncodeHeaders with a primed per-connection cache,
// fresh permutation and a mutated value per request, both enumeration passes
// running. Compare against BenchmarkEncodeHeadersOrder.
func BenchmarkEncodeHeadersOrderCached(b *testing.B) {
	header, order := benchShape()
	perms := benchPermutations(order, 64)
	tokens := make([]string, 16)
	for i := range tokens {
		tokens[i] = "Bearer " + strconv.Itoa(i) + "123456789abcdef0123456789abcdef"
	}
	param := testParam(header, order)
	param.Request.Method = "GET"
	param.Request.ActualContentLength = 0
	param.PlanCache = NewHeaderPlanCache()
	sink := func(name, value string) {}
	for range 2 {
		if _, err := EncodeHeaders(b.Context(), param, sink); err != nil {
			b.Fatal(err)
		}
	}
	if param.PlanCache.hits != 0 || len(param.PlanCache.shapes) != 1 {
		b.Fatalf("cache not primed: hits = %d, shapes = %d", param.PlanCache.hits, len(param.PlanCache.shapes))
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		param.HeaderOrder = perms[i%len(perms)]
		header["Authorization"][0] = tokens[i%len(tokens)]
		if _, err := EncodeHeaders(b.Context(), param, sink); err != nil {
			b.Fatal(err)
		}
		i++
	}
}

// The bound for a caller whose shape never repeats: every request is a
// distinct shape, so nothing is ever admitted and every call pays the
// identity hash on top of the ordinary build. Run with cache=false for the
// same workload's uncached cost.
func BenchmarkEncodeHeadersOrderShifting(b *testing.B) {
	for _, cached := range []bool{false, true} {
		name := "nocache"
		if cached {
			name = "cache"
		}
		b.Run(name, func(b *testing.B) {
			header, order := benchShape()
			shapes := make([]EncodeHeadersParam, 3*headerPlanShapes)
			var cache *HeaderPlanCache
			if cached {
				cache = NewHeaderPlanCache()
			}
			for i := range shapes {
				h := make(map[string][]string, len(header)+1)
				for k, v := range header {
					h[k] = v
				}
				extra := "X-Shape-" + strconv.Itoa(i)
				h[extra] = []string{"v"}
				o := append(slices.Clone(order), "x-shape-"+strconv.Itoa(i))
				shapes[i] = testParam(h, o)
				shapes[i].Request.Method = "GET"
				shapes[i].Request.ActualContentLength = 0
				shapes[i].PlanCache = cache
			}
			sink := func(name, value string) {}
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				if _, err := EncodeHeaders(b.Context(), shapes[i%len(shapes)], sink); err != nil {
					b.Fatal(err)
				}
				i++
			}
			if cache != nil && cache.hits > 0 {
				b.Fatalf("shifting workload hit the cache %d times; the bench no longer measures the miss bound", cache.hits)
			}
		})
	}
}

// TestPlanCacheZeroValue checks a cache built without the constructor seeds
// itself instead of panicking inside maphash.
func TestPlanCacheZeroValue(t *testing.T) {
	var c HeaderPlanCache
	h := map[string][]string{"Accept": {"a"}}
	order := []string{"accept"}
	for round := 0; round < 3; round++ {
		want, orderedLen := planHeaderOrderRef(order, h)
		got := clonePlan(c.plan(order, h))
		comparePlans(t, fmt.Sprintf("round %d", round), got, want, orderedLen)
	}
	if c.hits != 1 {
		t.Errorf("hits = %d after three identical calls, want 1", c.hits)
	}
}
