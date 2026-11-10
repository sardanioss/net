package httpcommon

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

// zeroLengthFields encodes a request with the given method and body length
// and returns its regular fields in wire order.
func zeroLengthFields(t *testing.T, method string, length int64, methods []string) []string {
	t.Helper()
	u, _ := url.Parse("https://example.com/")
	var got []string
	_, err := EncodeHeaders(context.Background(), EncodeHeadersParam{
		Request: Request{
			URL:                 u,
			Method:              method,
			Host:                "example.com",
			Header:              map[string][]string{"Accept": {"*/*"}, "X-A": {"1"}},
			ActualContentLength: length,
		},
		DefaultUserAgent:         "test-agent",
		HeaderOrder:              []string{"accept", "content-length", "x-a"},
		ZeroContentLengthMethods: methods,
	}, func(name, value string) {
		if !strings.HasPrefix(name, ":") {
			got = append(got, name+": "+value)
		}
	})
	if err != nil {
		t.Fatalf("EncodeHeaders: %v", err)
	}
	return got
}

// Which methods declare an empty body is the caller's to say: browsers do
// not agree with net/http, nor with each other. The header keeps the slot the
// order gives it, and a list never touches a request that has a body or one
// whose length is unknown.
func TestZeroContentLengthMethods(t *testing.T) {
	var (
		with    = []string{"accept: */*", "content-length: 0", "x-a: 1", "user-agent: test-agent"}
		without = []string{"accept: */*", "x-a: 1", "user-agent: test-agent"}
		safari  = []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS"}
		chrome  = []string{"POST", "PUT"}
	)
	for _, tt := range []struct {
		name    string
		methods []string
		sends   string
	}{
		{"net/http's, when nil", nil, "POST PUT PATCH"},
		{"Safari's", safari, "POST PUT PATCH DELETE OPTIONS"},
		{"Chrome's", chrome, "POST PUT"},
		{"none, when empty", []string{}, ""},
	} {
		for _, method := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
			want := without
			if strings.Contains(tt.sends, method) {
				want = with
			}
			t.Run(tt.name+"/"+method, func(t *testing.T) {
				eq(t, zeroLengthFields(t, method, 0, tt.methods), want)
			})
		}
	}

	eq(t, zeroLengthFields(t, "DELETE", -1, safari), without)
	eq(t, zeroLengthFields(t, "PATCH", 7, chrome),
		[]string{"accept: */*", "content-length: 7", "x-a: 1", "user-agent: test-agent"})
}
