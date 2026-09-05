package httpcommon

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

// encodePseudo runs EncodeHeaders with a custom pseudo-header order and
// returns only the pseudo-header fields, in wire order.
func encodePseudo(t *testing.T, pseudoOrder []string) []string {
	t.Helper()
	u, _ := url.Parse("https://example.com/p?q=1")
	var got []string
	_, err := EncodeHeaders(context.Background(), EncodeHeadersParam{
		Request: Request{
			URL:    u,
			Method: "GET",
			Host:   "example.com",
			Header: map[string][]string{},
		},
		DefaultUserAgent:  "test-agent",
		PseudoHeaderOrder: pseudoOrder,
	}, func(name, value string) {
		if strings.HasPrefix(name, ":") {
			got = append(got, name+"="+value)
		}
	})
	if err != nil {
		t.Fatalf("EncodeHeaders: %v", err)
	}
	return got
}

// A custom pseudo-header order is followed as given, and a name that is not a
// pseudo-header the request carries is skipped without effect.
func TestCustomPseudoHeaderOrder(t *testing.T) {
	got := encodePseudo(t, []string{":method", ":path", ":authority", ":scheme", ":unknown"})
	eq(t, got, []string{":method=GET", ":path=/p?q=1", ":authority=example.com", ":scheme=https"})
}

// A custom order that does not name a pseudo-header does not emit it.
func TestCustomPseudoHeaderOrderOmits(t *testing.T) {
	got := encodePseudo(t, []string{":method", ":authority"})
	eq(t, got, []string{":method=GET", ":authority=example.com"})
}
