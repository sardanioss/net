package hpack

import (
	"bytes"
	"testing"
)

// Representations is an override layer. A browser profile leaves it empty
// because its base policy is already right; it carries only the names where a
// mirrored client demonstrably differs. These lock each choice against the
// byte it must produce.
func TestPinnedRepresentations(t *testing.T) {
	for _, tc := range []struct {
		name string
		rep  Representation
		want string
	}{
		{"never", RepresentationNever, "never-indexed"},
		{"without", RepresentationWithout, "without-indexing"},
		{"incremental", RepresentationIncremental, "incremental"},
	} {
		var buf bytes.Buffer
		e := NewEncoder(&buf)
		e.SetHeaderRepresentations(map[string]Representation{"authorization": tc.rep})
		got := repr(t, encodeOne(t, e, &buf, HeaderField{Name: "authorization", Value: "Bearer x"}))
		if got != tc.want {
			t.Errorf("%s: authorization went out as %s, want %s", tc.name, got, tc.want)
		}
	}
}

// RFC 7541 6.2.2 is a literal representation, so an exact table match must not
// turn it into an indexed reference. searchTable only skips its name-and-value
// lookup for sensitive fields, so "without" has to suppress it explicitly.
// accept-encoding: gzip, deflate is static index 16 and exercises that path.
func TestWithoutIndexingBeatsAnExactTableMatch(t *testing.T) {
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	e.SetHeaderRepresentations(map[string]Representation{"accept-encoding": RepresentationWithout})
	got := repr(t, encodeOne(t, e, &buf, HeaderField{Name: "accept-encoding", Value: "gzip, deflate"}))
	if got != "without-indexing" {
		t.Errorf("exact static-table match went out as %s, want without-indexing", got)
	}
}

// "without" must also keep the field out of the dynamic table, or the second
// send comes back as an indexed reference and the pin silently stops holding.
func TestWithoutIndexingNeverEntersTheDynamicTable(t *testing.T) {
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	e.SetHeaderRepresentations(map[string]Representation{"x-token": RepresentationWithout})
	f := HeaderField{Name: "x-token", Value: "abc"}
	first := repr(t, encodeOne(t, e, &buf, f))
	second := repr(t, encodeOne(t, e, &buf, f))
	if first != "without-indexing" || second != "without-indexing" {
		t.Errorf("representations were %s then %s, want without-indexing twice", first, second)
	}
}

// An empty map must change nothing. This is the property that lets a browser
// profile ship without an entry, and it guards the Chrome parity work: Chrome
// indexes cookie and authorization like any other header.
func TestEmptyRepresentationMapChangesNothing(t *testing.T) {
	for _, name := range []string{"cookie", "authorization"} {
		var buf bytes.Buffer
		e := NewEncoder(&buf)
		e.SetHeaderRepresentations(nil)
		got := repr(t, encodeOne(t, e, &buf, HeaderField{Name: name, Value: "v"}))
		if got != "incremental" {
			t.Errorf("%s went out as %s with no override, want incremental", name, got)
		}
	}
}

// A pin has to outrank the policy knobs, otherwise an override is not an
// override.
func TestRepresentationOutranksTheIndexingControls(t *testing.T) {
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	e.SetAlwaysIndexHeaders([]string{"authorization"})
	e.SetCustomIndexingFunc(func(HeaderField) bool { return true })
	e.SetHeaderRepresentations(map[string]Representation{"authorization": RepresentationNever})
	got := repr(t, encodeOne(t, e, &buf, HeaderField{Name: "authorization", Value: "Bearer x"}))
	if got != "never-indexed" {
		t.Errorf("pinned representation lost to the indexing controls: got %s", got)
	}
}

func TestParseRepresentation(t *testing.T) {
	for in, want := range map[string]Representation{
		"":              RepresentationDefault,
		"default":       RepresentationDefault,
		"incremental":   RepresentationIncremental,
		"indexed":       RepresentationIncremental,
		"without":       RepresentationWithout,
		"never":         RepresentationNever,
		"never_indexed": RepresentationNever,
	} {
		got, ok := ParseRepresentation(in)
		if !ok || got != want {
			t.Errorf("ParseRepresentation(%q) = %v, %v; want %v, true", in, got, ok, want)
		}
	}
	if _, ok := ParseRepresentation("nonsense"); ok {
		t.Error("an unknown representation must be rejected, not silently defaulted")
	}
}
