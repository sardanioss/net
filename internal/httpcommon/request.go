// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package httpcommon

import (
	"context"
	"errors"
	"fmt"
	"github.com/sardanioss/http/httptrace"
	"net/textproto"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/sardanioss/net/http/httpguts"
	"github.com/sardanioss/net/http2/hpack"
)

var (
	ErrRequestHeaderListSize = errors.New("request header list larger than peer's advertised limit")
)

// Request is a subset of http.Request.
// It'd be simpler to pass an *http.Request, of course, but we can't depend on net/http
// without creating a dependency cycle.
type Request struct {
	URL                 *url.URL
	Method              string
	Host                string
	Header              map[string][]string
	Trailer             map[string][]string
	ActualContentLength int64 // 0 means 0, -1 means unknown
}

// EncodeHeadersParam is parameters to EncodeHeaders.
type EncodeHeadersParam struct {
	Request Request

	// AddGzipHeader indicates that an "accept-encoding: gzip" header should be
	// added to the request.
	AddGzipHeader bool

	// PeerMaxHeaderListSize, when non-zero, is the peer's MAX_HEADER_LIST_SIZE setting.
	PeerMaxHeaderListSize uint64

	// DefaultUserAgent is the User-Agent header to send when the request
	// neither contains a User-Agent nor disables it.
	DefaultUserAgent string

	// PseudoHeaderOrder specifies the order of HTTP/2 pseudo-headers.
	// Default order is [":method", ":authority", ":scheme", ":path"] (Chrome order).
	// If nil, uses Chrome order. Firefox uses [":method", ":path", ":authority", ":scheme"].
	PseudoHeaderOrder []string

	// HeaderOrder specifies the order in which regular headers should be sent.
	// If nil, headers are sent in sorted order.
	HeaderOrder []string

	// DisableCookieSplit, when true, sends the Cookie header as a single
	// HPACK entry instead of splitting on semicolons per RFC 9113 §8.2.3.
	// Chrome sends cookies as one entry; splitting is detectable by servers.
	DisableCookieSplit bool

	// PlanCache, when non-nil, caches header-order resolution metadata
	// across calls, for callers that send the same header shape on many
	// requests. It never changes what is sent: a cached resolution is fully
	// verified against the request before use and any divergence falls back
	// to the ordinary build. The cache is not safe for concurrent use, so a
	// caller must not share one between two EncodeHeaders calls that can
	// overlap; the http2 transport keeps one per connection and encodes
	// under the connection's write lock.
	PlanCache *HeaderPlanCache
}

// EncodeHeadersResult is the result of EncodeHeaders.
type EncodeHeadersResult struct {
	HasBody     bool
	HasTrailers bool
}

// EncodeHeaders constructs request headers common to HTTP/2 and HTTP/3.
// It validates a request and calls headerf with each pseudo-header and header
// for the request.
// The headerf function is called with the validated, canonicalized header name.
func EncodeHeaders(ctx context.Context, param EncodeHeadersParam, headerf func(name, value string)) (res EncodeHeadersResult, _ error) {
	req := param.Request

	// Check for invalid connection-level headers.
	if err := checkConnHeaders(req.Header); err != nil {
		return res, err
	}

	if req.URL == nil {
		return res, errors.New("Request.URL is nil")
	}

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	host, err := httpguts.PunycodeHostPort(host)
	if err != nil {
		return res, err
	}
	if !httpguts.ValidHostHeader(host) {
		return res, errors.New("invalid Host header")
	}

	// isNormalConnect is true if this is a non-extended CONNECT request.
	isNormalConnect := false
	var protocol string
	if vv := req.Header[":protocol"]; len(vv) > 0 {
		protocol = vv[0]
	}
	if req.Method == "CONNECT" && protocol == "" {
		isNormalConnect = true
	} else if protocol != "" && req.Method != "CONNECT" {
		return res, errors.New("invalid :protocol header in non-CONNECT request")
	}

	// Validate the path, except for non-extended CONNECT requests which have no path.
	var path string
	if !isNormalConnect {
		path = req.URL.RequestURI()
		if !validPseudoPath(path) {
			orig := path
			path = strings.TrimPrefix(path, req.URL.Scheme+"://"+host)
			if !validPseudoPath(path) {
				if req.URL.Opaque != "" {
					return res, fmt.Errorf("invalid request :path %q from URL.Opaque = %q", orig, req.URL.Opaque)
				} else {
					return res, fmt.Errorf("invalid request :path %q", orig)
				}
			}
		}
	}

	// Check for any invalid headers+trailers and return an error before we
	// potentially pollute our hpack state. (We want to be able to
	// continue to reuse the hpack encoder for future requests)
	if err := validateHeaders(req.Header); err != "" {
		return res, fmt.Errorf("invalid HTTP header %s", err)
	}
	if err := validateHeaders(req.Trailer); err != "" {
		return res, fmt.Errorf("invalid HTTP trailer %s", err)
	}

	trailers, err := commaSeparatedTrailers(req.Trailer)
	if err != nil {
		return res, err
	}

	// A non-empty order list is resolved before enumeration because
	// enumerateHeaders can run twice per request, once counting bytes against
	// the peer's header list size and once encoding, and resolution is the
	// expensive half: every order entry has to find the key req.Header
	// actually stores its header under, whatever the casing. Both passes
	// replay the resolved plan.
	var orderPlan []plannedHeader
	if len(param.HeaderOrder) > 0 {
		if param.PlanCache != nil {
			orderPlan = param.PlanCache.plan(param.HeaderOrder, req.Header)
		} else {
			orderPlan = planHeaderOrder(param.HeaderOrder, req.Header)
		}
	}

	// 8.1.2.3 Request Pseudo-Header Fields
	// The :path pseudo-header field includes the path and query parts of the
	// target URI (the path-absolute production and optionally a '?' character
	// followed by the query production, see Sections 3.3 and 3.4 of
	// [RFC3986]).
	m := req.Method
	if m == "" {
		m = "GET"
	}

	// pseudoValue returns the value the request carries for a pseudo-header,
	// replacing a map both enumeration passes used to rebuild. Names not
	// matched here are skipped, exactly as a map miss was.
	pseudoValue := func(name string) (string, bool) {
		switch name {
		case ":method":
			return m, true
		case ":authority":
			return host, true
		case ":path":
			if !isNormalConnect {
				return path, true
			}
		case ":scheme":
			if !isNormalConnect {
				return req.URL.Scheme, true
			}
		case ":protocol":
			if protocol != "" {
				return protocol, true
			}
		}
		return "", false
	}

	// Send pseudo-headers in specified order (default: Chrome order)
	pseudoOrder := param.PseudoHeaderOrder
	if len(pseudoOrder) == 0 {
		// Default to Chrome order: :method, :authority, :scheme, :path
		switch {
		case isNormalConnect:
			pseudoOrder = defaultPseudoOrderConnect
		case protocol != "":
			pseudoOrder = defaultPseudoOrderProtocol
		default:
			pseudoOrder = defaultPseudoOrder
		}
	}

	// enumerateHeaders calls f for every field the request sends, in wire
	// order. wireName is the ASCII-lowered name when the emission site already
	// has it: literal names below are written lowercase and the order plan
	// carries the lowered form of every key it resolved. An empty wireName
	// means the caller must lower (and validate) the name itself, which only
	// the unordered fallback path still needs.
	enumerateHeaders := func(f func(name, wireName, value string)) {
		for _, name := range pseudoOrder {
			if val, ok := pseudoValue(name); ok {
				f(name, name, val)
			}
		}
		if trailers != "" {
			f("trailer", "trailer", trailers)
		}

		var didUA bool
		var didContentLength bool

		// Helper to process a single header
		processHeader := func(k, wireName string, vv []string) {
			// Skip magic ordering keys - they are only for controlling order
			if k == "Header-Order:" || k == "PHeader-Order:" {
				return
			}
			if asciiEqualFold(k, "host") {
				// Host is :authority, already sent.
				return
			} else if asciiEqualFold(k, "content-length") {
				// Content-Length is handled separately - either via HeaderOrder loop
				// or at the end. Never send it from processHeader.
				return
			} else if asciiEqualFold(k, "connection") ||
				asciiEqualFold(k, "proxy-connection") ||
				asciiEqualFold(k, "transfer-encoding") ||
				asciiEqualFold(k, "upgrade") ||
				asciiEqualFold(k, "keep-alive") {
				// Per 8.1.2.2 Connection-Specific Header
				// Fields, don't send connection-specific
				// fields. We have already checked if any
				// are error-worthy so just ignore the rest.
				return
			} else if asciiEqualFold(k, "user-agent") {
				// Match Go's http1 behavior: at most one
				// User-Agent. If set to nil or empty string,
				// then omit it. Otherwise if not mentioned,
				// include the default (below).
				didUA = true
				if len(vv) < 1 {
					return
				}
				vv = vv[:1]
				if vv[0] == "" {
					return
				}
			} else if asciiEqualFold(k, "cookie") {
				if !param.DisableCookieSplit {
					// Per 8.1.2.5 To allow for better compression efficiency, the
					// Cookie header field MAY be split into separate header fields,
					// each with one or more cookie-pairs.
					for _, v := range vv {
						for {
							p := strings.IndexByte(v, ';')
							if p < 0 {
								break
							}
							f("cookie", "cookie", v[:p])
							p++
							// strip space after semicolon if any.
							for p+1 <= len(v) && v[p] == ' ' {
								p++
							}
							v = v[p:]
						}
						if len(v) > 0 {
							f("cookie", "cookie", v)
						}
					}
					return
				}
				// DisableCookieSplit: fall through to send full cookie as one entry
			} else if k == ":protocol" {
				// :protocol pseudo-header was already sent above.
				return
			}

			for _, v := range vv {
				f(k, wireName, v)
			}
		}

		// Emit the ordered headers if HeaderOrder is provided
		if orderPlan != nil {
			for _, ph := range orderPlan {
				if ph.contentLength {
					if !didContentLength && shouldSendReqContentLength(req.Method, req.ActualContentLength) {
						f("content-length", "content-length", strconv.FormatInt(req.ActualContentLength, 10))
						didContentLength = true
					}
					continue
				}
				processHeader(ph.key, ph.wireName, ph.values)
			}
		} else {
			// No order specified, use sorted order for consistency
			keys := make([]string, 0, len(req.Header))
			for k := range req.Header {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				processHeader(k, "", req.Header[k])
			}
		}
		// Only send content-length here if not already sent via HeaderOrder
		if !didContentLength && shouldSendReqContentLength(req.Method, req.ActualContentLength) {
			f("content-length", "content-length", strconv.FormatInt(req.ActualContentLength, 10))
		}
		if param.AddGzipHeader {
			f("accept-encoding", "accept-encoding", "gzip")
		}
		if !didUA {
			f("user-agent", "user-agent", param.DefaultUserAgent)
		}
	}

	// Do a first pass over the headers counting bytes to ensure
	// we don't exceed cc.peerMaxHeaderListSize. This is done as a
	// separate pass before encoding the headers to prevent
	// modifying the hpack state.
	if param.PeerMaxHeaderListSize > 0 {
		hlSize := uint64(0)
		enumerateHeaders(func(name, _, value string) {
			hf := hpack.HeaderField{Name: name, Value: value}
			hlSize += uint64(hf.Size())
		})

		if hlSize > param.PeerMaxHeaderListSize {
			return res, ErrRequestHeaderListSize
		}
	}

	trace := httptrace.ContextClientTrace(ctx)

	// Header list size is ok. Write the headers.
	enumerateHeaders(func(name, wireName, value string) {
		if wireName == "" {
			var ascii bool
			wireName, ascii = LowerHeader(name)
			if !ascii {
				// Skip writing invalid headers. Per RFC 7540, Section 8.1.2, header
				// field names have to be ASCII characters (just as in HTTP/1.x).
				// A name arriving with wireName set never trips this: it came off
				// the order plan, and validateHeaders vetted every planned name
				// above before the plan was built.
				return
			}
		}

		headerf(wireName, value)

		if trace != nil && trace.WroteHeaderField != nil {
			trace.WroteHeaderField(wireName, []string{value})
		}
	})

	res.HasBody = req.ActualContentLength != 0
	res.HasTrailers = trailers != ""
	return res, nil
}

// hasHeaderFold reports whether the map holds the named header under any
// spelling of its key.
//
// http.Header.Get canonicalises before looking up, so a map built through
// Header.Set can be indexed directly. A map built by assigning to it cannot:
// header["accept-encoding"] and header["Accept-Encoding"] are different keys.
// That distinction matters here because a caller reproducing a captured request
// stores header names exactly as the wire carried them, and on HTTP/2 the wire
// carries them lowercased.
func hasHeaderFold(header map[string][]string, name string) bool {
	if len(header[name]) > 0 {
		return true
	}
	for k, v := range header {
		if len(v) > 0 && asciiEqualFold(k, name) {
			return true
		}
	}
	return false
}

// IsRequestGzip reports whether we should add an Accept-Encoding: gzip header
// for a request.
func IsRequestGzip(method string, header map[string][]string, disableCompression bool) bool {
	// TODO(bradfitz): this is a copy of the logic in net/http. Unify somewhere?
	//
	// The two lookups fold case. Indexing the map directly missed a header the
	// caller had set under a non-canonical key, so a request already carrying
	// accept-encoding got a second one appended and went out with the header
	// twice, which no browser does. Same for Range, where the duplicate also
	// defeats the reason gzip is skipped for ranged requests.
	if !disableCompression &&
		!hasHeaderFold(header, "Accept-Encoding") &&
		!hasHeaderFold(header, "Range") &&
		method != "HEAD" {
		// Request gzip only, not deflate. Deflate is ambiguous and
		// not as universally supported anyway.
		// See: https://zlib.net/zlib_faq.html#faq39
		//
		// Note that we don't request this for HEAD requests,
		// due to a bug in nginx:
		//   http://trac.nginx.org/nginx/ticket/358
		//   https://golang.org/issue/5522
		//
		// We don't request gzip if the request is for a range, since
		// auto-decoding a portion of a gzipped document will just fail
		// anyway. See https://golang.org/issue/8923
		return true
	}
	return false
}

// checkConnHeaders checks whether req has any invalid connection-level headers.
//
// https://www.rfc-editor.org/rfc/rfc9114.html#section-4.2-3
// https://www.rfc-editor.org/rfc/rfc9113.html#section-8.2.2-1
//
// Certain headers are special-cased as okay but not transmitted later.
// For example, we allow "Transfer-Encoding: chunked", but drop the header when encoding.
func checkConnHeaders(h map[string][]string) error {
	if vv := h["Upgrade"]; len(vv) > 0 && (vv[0] != "" && vv[0] != "chunked") {
		return fmt.Errorf("invalid Upgrade request header: %q", vv)
	}
	if vv := h["Transfer-Encoding"]; len(vv) > 0 && (len(vv) > 1 || vv[0] != "" && vv[0] != "chunked") {
		return fmt.Errorf("invalid Transfer-Encoding request header: %q", vv)
	}
	if vv := h["Connection"]; len(vv) > 0 && (len(vv) > 1 || vv[0] != "" && !asciiEqualFold(vv[0], "close") && !asciiEqualFold(vv[0], "keep-alive")) {
		return fmt.Errorf("invalid Connection request header: %q", vv)
	}
	return nil
}

func commaSeparatedTrailers(trailer map[string][]string) (string, error) {
	keys := make([]string, 0, len(trailer))
	for k := range trailer {
		k = CanonicalHeader(k)
		switch k {
		case "Transfer-Encoding", "Trailer", "Content-Length":
			return "", fmt.Errorf("invalid Trailer key %q", k)
		}
		keys = append(keys, k)
	}
	if len(keys) > 0 {
		sort.Strings(keys)
		return strings.Join(keys, ","), nil
	}
	return "", nil
}

// validPseudoPath reports whether v is a valid :path pseudo-header
// value. It must be either:
//
//   - a non-empty string starting with '/'
//   - the string '*', for OPTIONS requests.
//
// For now this is only used a quick check for deciding when to clean
// up Opaque URLs before sending requests from the Transport.
// See golang.org/issue/16847
//
// We used to enforce that the path also didn't start with "//", but
// Google's GFE accepts such paths and Chrome sends them, so ignore
// that part of the spec. See golang.org/issue/19103.
func validPseudoPath(v string) bool {
	return (len(v) > 0 && v[0] == '/') || v == "*"
}

func validateHeaders(hdrs map[string][]string) string {
	for k, vv := range hdrs {
		// Skip magic ordering keys - they are not sent over the wire
		if k == "Header-Order:" || k == "PHeader-Order:" {
			continue
		}
		if !httpguts.ValidHeaderFieldName(k) && k != ":protocol" {
			return fmt.Sprintf("name %q", k)
		}
		for _, v := range vv {
			if !httpguts.ValidHeaderFieldValue(v) {
				// Don't include the value in the error,
				// because it may be sensitive.
				return fmt.Sprintf("value for header %q", k)
			}
		}
	}
	return ""
}

// shouldSendReqContentLength reports whether we should send
// a "content-length" request header. This logic is basically a copy of the net/http
// transferWriter.shouldSendContentLength.
// The contentLength is the corrected contentLength (so 0 means actually 0, not unknown).
// -1 means unknown.
func shouldSendReqContentLength(method string, contentLength int64) bool {
	if contentLength > 0 {
		return true
	}
	if contentLength < 0 {
		return false
	}
	// For zero bodies, whether we send a content-length depends on the method.
	// It also kinda doesn't matter for http2 either way, with END_STREAM.
	switch method {
	case "POST", "PUT", "PATCH":
		return true
	default:
		return false
	}
}

// ServerRequestParam is parameters to NewServerRequest.
type ServerRequestParam struct {
	Method                  string
	Scheme, Authority, Path string
	Protocol                string
	Header                  map[string][]string
}

// ServerRequestResult is the result of NewServerRequest.
type ServerRequestResult struct {
	// Various http.Request fields.
	URL        *url.URL
	RequestURI string
	Trailer    map[string][]string

	NeedsContinue bool // client provided an "Expect: 100-continue" header

	// If the request should be rejected, this is a short string suitable for passing
	// to the http2 package's CountError function.
	// It might be a bit odd to return errors this way rather than returning an error,
	// but this ensures we don't forget to include a CountError reason.
	InvalidReason string
}

func NewServerRequest(rp ServerRequestParam) ServerRequestResult {
	needsContinue := httpguts.HeaderValuesContainsToken(rp.Header["Expect"], "100-continue")
	if needsContinue {
		delete(rp.Header, "Expect")
	}
	// Merge Cookie headers into one "; "-delimited value.
	if cookies := rp.Header["Cookie"]; len(cookies) > 1 {
		rp.Header["Cookie"] = []string{strings.Join(cookies, "; ")}
	}

	// Setup Trailers
	var trailer map[string][]string
	for _, v := range rp.Header["Trailer"] {
		for _, key := range strings.Split(v, ",") {
			key = textproto.CanonicalMIMEHeaderKey(textproto.TrimString(key))
			switch key {
			case "Transfer-Encoding", "Trailer", "Content-Length":
				// Bogus. (copy of http1 rules)
				// Ignore.
			default:
				if trailer == nil {
					trailer = make(map[string][]string)
				}
				trailer[key] = nil
			}
		}
	}
	delete(rp.Header, "Trailer")

	// "':authority' MUST NOT include the deprecated userinfo subcomponent
	// for "http" or "https" schemed URIs."
	// https://www.rfc-editor.org/rfc/rfc9113.html#section-8.3.1-2.3.8
	if strings.IndexByte(rp.Authority, '@') != -1 && (rp.Scheme == "http" || rp.Scheme == "https") {
		return ServerRequestResult{
			InvalidReason: "userinfo_in_authority",
		}
	}

	var url_ *url.URL
	var requestURI string
	if rp.Method == "CONNECT" && rp.Protocol == "" {
		url_ = &url.URL{Host: rp.Authority}
		requestURI = rp.Authority // mimic HTTP/1 server behavior
	} else {
		var err error
		url_, err = url.ParseRequestURI(rp.Path)
		if err != nil {
			return ServerRequestResult{
				InvalidReason: "bad_path",
			}
		}
		requestURI = rp.Path
	}

	return ServerRequestResult{
		URL:           url_,
		NeedsContinue: needsContinue,
		RequestURI:    requestURI,
		Trailer:       trailer,
	}
}

// Default pseudo-header emission orders. Chrome sends
// :method, :authority, :scheme, :path; a non-extended CONNECT has no
// :scheme or :path, and an extended CONNECT carries :protocol last.
var (
	defaultPseudoOrder         = []string{":method", ":authority", ":scheme", ":path"}
	defaultPseudoOrderConnect  = []string{":method", ":authority"}
	defaultPseudoOrderProtocol = []string{":method", ":authority", ":scheme", ":path", ":protocol"}
)

// foldKey returns the ASCII-lowered form of a header name, the canonical
// representative of its fold class: foldKey(a) == foldKey(b) exactly when
// asciiEqualFold(a, b), for any byte strings, because both touch only 'A'-'Z'.
// An already-lowercase name is returned as is after nothing but the scan, and
// a well-known canonical name resolves through the interned table, so neither
// allocates. The table is only consulted once an uppercase byte proves the
// scan cannot succeed; every interned key is canonical cased, so the scan
// never skips past a name the table holds.
func foldKey(s string) string {
	for i := 0; i < len(s); i++ {
		if 'A' <= s[i] && s[i] <= 'Z' {
			buildCommonHeaderMapsOnce()
			if l, ok := commonLowerHeader[s]; ok {
				return l
			}
			b := []byte(s)
			for ; i < len(b); i++ {
				b[i] = lower(b[i])
			}
			return string(b)
		}
	}
	return s
}

// plannedHeader is one emission the order plan produced: a resolved header
// with the values its slot takes, or the slot at which content-length goes
// out. Content-length is not in req.Header and its value is resolved at
// emission time, so the plan only records its position. wireName is the
// ASCII-lowered key, computed once while the fold index was built, so the
// encode pass does not lower the same name again.
type plannedHeader struct {
	key           string
	wireName      string
	values        []string
	contentLength bool
}

// planHeaderOrder resolves a header order list against h once, into the exact
// sequence of emissions the old per-pass resolution produced.
//
// Resolution finds the key h actually stores each ordered name under. The
// exact key wins over a fold match, which matters only when a caller lists
// one name in two casings: "Cookie" and "cookie" are two map entries, and a
// fold match over a randomised map iteration would point both order slots
// at whichever one it happened to reach first. With more than one fold
// candidate and no exact hit, take the lowest, so the wire order does not
// change from request to request.
//
// A name may hold more than one slot in the order list, which is how a caller
// asks for two fields of one name in chosen positions relative to other
// names: cookie, accept, cookie has to keep the accept in the middle. Count
// the slots per resolved key first, then hand slot i the value at index i.
// Headers h holds that the order list never named follow in map order; which
// order that is varies per request but not between the two enumeration
// passes, which replay one plan.
//
// The map keys live in a slice of entries and one index finds an entry by
// exact key or by fold class, so a name resolves in one lookup and the slot
// and cursor counts are plain ints on the entry rather than two more maps.
// For the common caller, whose order names are already lowercase, the exact
// key and the fold class share an index slot and even a fold resolution is a
// single lookup.
func planHeaderOrder(order []string, h map[string][]string) []plannedHeader {
	return planHeaderOrderInto(order, h, nil)
}

// headerEntry is one header map entry under resolution: its exact key, the
// ASCII-lowered form, the values, and the slot bookkeeping. slots and cursor
// are int32 to keep the entry at 64 bytes; overflowing them would take an
// order list two billion entries long.
type headerEntry struct {
	key    string
	lower  string
	values []string
	slots  int32
	cursor int32
}

// indexEntry finds a headerEntry by exact key or by fold class. Values are
// the entry index + 1, so absent and entry 0 stay distinct.
type indexEntry struct {
	exact int32
	fold  int32
}

// planHeaderOrderInto is planHeaderOrder with its working storage taken from
// scratch when scratch is non-nil, so a caching caller reuses one allocation
// set across requests. In that case the returned plan aliases scratch.plan
// and is valid until the next call with the same scratch, and the entry and
// resolution slices are left behind for the caller to read.
func planHeaderOrderInto(order []string, h map[string][]string, scratch *planScratch) []plannedHeader {
	var entries []headerEntry
	var index map[string]indexEntry
	if scratch == nil {
		entries = make([]headerEntry, 0, len(h))
		index = make(map[string]indexEntry, 2*len(h))
	} else {
		entries = scratch.entries[:0]
		if scratch.index == nil {
			scratch.index = make(map[string]indexEntry, 2*len(h))
		} else {
			clear(scratch.index)
		}
		index = scratch.index
	}
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

	// Resolve every order name once. resolved holds the entry index + 1 per
	// order slot, 0 for a name h does not hold and -1 for a content-length
	// slot, which is not resolved against h at all.
	var resolved []int32
	if scratch == nil {
		resolved = make([]int32, len(order))
	} else {
		if cap(scratch.resolved) < len(order) {
			scratch.resolved = make([]int32, len(order))
		} else {
			scratch.resolved = scratch.resolved[:len(order)]
			clear(scratch.resolved)
		}
		resolved = scratch.resolved
	}
	for oi, k := range order {
		// Special case: content-length is not in req.Header but we handle it
		if asciiEqualFold(k, "content-length") {
			resolved[oi] = -1
			continue
		}
		e, ok := index[k]
		if !ok || e.exact == 0 {
			// No exact entry. A fold class differing from the name itself can
			// only be reached through foldKey; a name that is its own fold
			// class already looked its class up.
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

	// An entry no order slot resolved to trails the plan in entry order,
	// which is map iteration order, as before.
	trailing := 0
	for i := range entries {
		if entries[i].slots == 0 {
			trailing++
		}
	}

	var plan []plannedHeader
	if scratch == nil {
		plan = make([]plannedHeader, 0, len(order)+trailing)
	} else {
		plan = scratch.plan[:0]
	}
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
	for i := range entries {
		if e := &entries[i]; e.slots == 0 {
			plan = append(plan, plannedHeader{key: e.key, wireName: e.lower, values: e.values})
		}
	}
	if scratch != nil {
		scratch.entries = entries
		scratch.plan = plan
	}
	return plan
}

// valuesForSlot returns the values one order slot emits.
//
// n is how many slots the order list gives this header name and i is which of
// them this is, counting from zero.
//
// One slot takes every value, which is the long-standing behaviour and the
// only shape an ordinary caller produces. Several slots split the values one
// apiece in order, and the last slot takes whatever is left, so listing a name
// twice against three values does not drop the third.
func valuesForSlot(vals []string, n, i int) []string {
	if n <= 1 {
		return vals
	}
	if i >= len(vals) {
		return nil
	}
	if i == n-1 {
		return vals[i:]
	}
	return vals[i : i+1]
}
