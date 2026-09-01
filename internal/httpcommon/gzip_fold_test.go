package httpcommon

import "testing"

// A caller reproducing a captured request stores header names as the wire
// carried them, and HTTP/2 carries them lowercased. Indexing the map directly
// missed that spelling, so a request already carrying accept-encoding got a
// second one appended and went out with the header twice.
func TestIsRequestGzipFoldsHeaderCase(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header map[string][]string
		want   bool
	}{
		{"no accept-encoding at all", map[string][]string{}, true},
		{"canonical key", map[string][]string{"Accept-Encoding": {"gzip"}}, false},
		{"lowercase key, as HTTP/2 carries it", map[string][]string{"accept-encoding": {"gzip, br"}}, false},
		{"shouty key", map[string][]string{"ACCEPT-ENCODING": {"gzip"}}, false},
		{"empty value is not a header", map[string][]string{"accept-encoding": {}}, true},
		{"lowercase range", map[string][]string{"range": {"bytes=0-1"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRequestGzip("GET", tc.header, false); got != tc.want {
				t.Errorf("IsRequestGzip = %v, want %v; a false here means the caller's "+
					"own header is about to be duplicated on the wire", got, tc.want)
			}
		})
	}
}
