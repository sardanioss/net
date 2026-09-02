package httpcommon

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

// encode runs EncodeHeaders and returns the non-pseudo fields in wire order as
// "name: value" strings.
func encode(t *testing.T, header map[string][]string, order []string) []string {
	t.Helper()
	u, _ := url.Parse("https://example.com/")
	var got []string
	_, err := EncodeHeaders(context.Background(), EncodeHeadersParam{
		Request: Request{
			URL:                 u,
			Method:              "GET",
			Host:                "example.com",
			Header:              header,
			ActualContentLength: 0,
		},
		DefaultUserAgent:   "test-agent",
		HeaderOrder:        order,
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
	return got
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("fields =\n  %v\nwant\n  %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fields =\n  %v\nwant\n  %v", got, want)
		}
	}
}

// A map cannot hold position, so the order list is what carries it. One slot
// per name meant a repeated name emitted all of its values at its first slot,
// which hoists the second cookie up next to the first and loses the accept that
// sat between them in the capture being replayed.
func TestRepeatedNameGetsOneSlotPerValue(t *testing.T) {
	got := encode(t,
		map[string][]string{"cookie": {"a=1", "b=2"}, "accept": {"*/*"}},
		[]string{"cookie", "accept", "cookie"})
	eq(t, got, []string{"cookie: a=1", "accept: */*", "cookie: b=2", "user-agent: test-agent"})
}

// The single-slot form has to keep emitting every value, because that is what
// every ordinary caller produces: the order list a preset builds is
// deduplicated, so a name never appears twice on that path.
func TestOneSlotStillEmitsEveryValue(t *testing.T) {
	got := encode(t,
		map[string][]string{"cookie": {"a=1", "b=2"}, "accept": {"*/*"}},
		[]string{"cookie", "accept"})
	eq(t, got, []string{"cookie: a=1", "cookie: b=2", "accept: */*", "user-agent: test-agent"})
}

// Fewer slots than values must not drop one. The last slot takes the rest.
func TestLastSlotTakesTheRemainder(t *testing.T) {
	got := encode(t,
		map[string][]string{"x-a": {"1", "2", "3"}, "x-b": {"z"}},
		[]string{"x-a", "x-b", "x-a"})
	eq(t, got, []string{"x-a: 1", "x-b: z", "x-a: 2", "x-a: 3", "user-agent: test-agent"})
}

// More slots than values emits nothing for the surplus rather than repeating
// the last value or panicking on the index.
func TestSurplusSlotsEmitNothing(t *testing.T) {
	got := encode(t,
		map[string][]string{"x-a": {"1"}, "x-b": {"z"}},
		[]string{"x-a", "x-b", "x-a"})
	eq(t, got, []string{"x-a: 1", "x-b: z", "user-agent: test-agent"})
}

// Two casings of one name are two map entries. Resolving by fold alone over a
// randomised map iteration pointed both slots at whichever entry came first, so
// the wire order changed from request to request. The exact key has to win.
func TestTwoCasingsResolveToTheirOwnEntries(t *testing.T) {
	for i := 0; i < 20; i++ {
		got := encode(t,
			map[string][]string{"Cookie": {"upper"}, "cookie": {"lower"}},
			[]string{"cookie", "Cookie"})
		eq(t, got, []string{"cookie: lower", "cookie: upper", "user-agent: test-agent"})
	}
}
