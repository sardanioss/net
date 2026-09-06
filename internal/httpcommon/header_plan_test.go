package httpcommon

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

// The common caller shape: the map was built through http.Header.Set, so its
// keys are canonical cased, while the order list is lowercase. Every entry
// must resolve through the fold index, and names the order lists but the map
// does not hold are skipped without effect.
func TestCanonicalMapResolvesLowercaseOrder(t *testing.T) {
	got := encode(t,
		map[string][]string{
			"Accept":          {"*/*"},
			"Accept-Encoding": {"gzip"},
			"X-Custom-Thing":  {"v"},
		},
		[]string{"accept", "x-absent", "x-custom-thing", "accept-encoding"})
	eq(t, got, []string{"accept: */*", "x-custom-thing: v", "accept-encoding: gzip", "user-agent: test-agent"})
}

// With two fold candidates and no exact hit, the lowest key wins, so the wire
// order does not change with map iteration order.
func TestLowestFoldCandidateWins(t *testing.T) {
	for i := 0; i < 20; i++ {
		got := encode(t,
			map[string][]string{"X-Thing": {"canon"}, "x-tHING": {"mixed"}},
			[]string{"x-thing", "x-thing"})
		eq(t, got, []string{"x-thing: canon", "x-thing: mixed", "user-agent: test-agent"})
	}
}

// A content-length slot in the order list places the header there, not at the
// tail, and only once.
func TestContentLengthKeepsItsSlot(t *testing.T) {
	u, _ := url.Parse("https://example.com/")
	var got []string
	_, err := EncodeHeaders(context.Background(), EncodeHeadersParam{
		Request: Request{
			URL:                 u,
			Method:              "POST",
			Host:                "example.com",
			Header:              map[string][]string{"Accept": {"*/*"}, "X-A": {"1"}},
			ActualContentLength: 42,
		},
		DefaultUserAgent:   "test-agent",
		HeaderOrder:        []string{"accept", "content-length", "x-a"},
		DisableCookieSplit: true,
	}, func(name, value string) {
		if strings.HasPrefix(name, ":") {
			return
		}
		got = append(got, name+": "+value)
	})
	if err != nil {
		t.Fatalf("EncodeHeaders: %v", err)
	}
	eq(t, got, []string{"accept: */*", "content-length: 42", "x-a: 1", "user-agent: test-agent"})
}

// foldKey partitions names exactly as asciiEqualFold does, non-ASCII bytes
// included, and hands back names that need no work without copying them.
func TestFoldKeyMatchesEqualFold(t *testing.T) {
	names := []string{
		"accept", "Accept", "ACCEPT", "aCCePt",
		"x-thing", "X-Thing", "x-thinG",
		"cookie", "Cookie",
		"x-f\xc3\xbc", "X-F\xc3\xbc", // non-ASCII tail, ASCII prefix folds
		"x-\xff", "X-\xff",
		"", "a", "A",
	}
	for _, a := range names {
		for _, b := range names {
			same := foldKey(a) == foldKey(b)
			if want := asciiEqualFold(a, b); same != want {
				t.Errorf("foldKey(%q)==foldKey(%q) = %v, asciiEqualFold = %v", a, b, same, want)
			}
		}
	}
	for _, s := range []string{"accept", "x-already-lower", ""} {
		if got := foldKey(s); got != s {
			t.Errorf("foldKey(%q) = %q, want the input unchanged", s, got)
		}
	}
}

// A production-shaped encode: a canonical-cased header map against a lowercase
// preset order that also names headers the request does not carry, with the
// peer max header list size set so both enumeration passes run.
func BenchmarkEncodeHeadersOrder(b *testing.B) {
	u, _ := url.Parse("https://example.com/some/path?x=1")
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
	param := EncodeHeadersParam{
		Request: Request{
			URL:                 u,
			Method:              "GET",
			Host:                "example.com",
			Header:              header,
			ActualContentLength: 0,
		},
		DefaultUserAgent:      "bench-agent/1.0",
		HeaderOrder:           order,
		DisableCookieSplit:    true,
		PeerMaxHeaderListSize: 262144,
	}
	sink := func(name, value string) {}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := EncodeHeaders(context.Background(), param, sink); err != nil {
			b.Fatal(err)
		}
	}
}
