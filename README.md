# sardanioss/net

A fork of `golang.org/x/net` with HTTP/2 fingerprinting support for evading bot detection systems.

## Why This Fork?

Modern anti-bot systems (Akamai, Cloudflare, PerimeterX) fingerprint HTTP/2 connections by analyzing:
- SETTINGS frame order and values
- WINDOW_UPDATE (connection flow) value
- Pseudo-header order (`:method`, `:authority`, `:scheme`, `:path`)
- Header priority in HEADERS frame
- PRIORITY frames
- HPACK dynamic table indexing behavior

The standard `golang.org/x/net/http2` package doesn't allow customizing these values, making Go HTTP clients easily detectable. This fork adds full control over all HTTP/2 fingerprint vectors.

## Installation

```bash
go get github.com/sardanioss/net
```

## Features

### HTTP/2 Transport Configuration

```go
import "github.com/sardanioss/net/http2"

transport := &http2.Transport{
    // Custom SETTINGS frame (order matters!)
    Settings: map[http2.SettingID]uint32{
        http2.SettingHeaderTableSize:   65536,
        http2.SettingEnablePush:        0,
        http2.SettingInitialWindowSize: 6291456,
        http2.SettingMaxHeaderListSize: 262144,
    },
    SettingsOrder: []http2.SettingID{
        http2.SettingHeaderTableSize,
        http2.SettingEnablePush,
        http2.SettingInitialWindowSize,
        http2.SettingMaxHeaderListSize,
    },

    // Custom WINDOW_UPDATE value (Chrome uses 15663105)
    ConnectionFlow: 15663105,

    // Pseudo-header order (Chrome: m,a,s,p)
    PseudoHeaderOrder: []string{":method", ":authority", ":scheme", ":path"},

    // Header priority in HEADERS frame
    HeaderPriority: &http2.PriorityParam{
        Weight:    255,  // 256 in Chrome (0-indexed)
        Exclusive: true,
        StreamDep: 0,
    },

    // Stream priority mode
    StreamPriorityMode: http2.StreamPriorityChrome,

    // Custom User-Agent
    UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
}
```

### HPACK Indexing Control

```go
import "github.com/sardanioss/net/http2/hpack"

// Configure via Transport
transport := &http2.Transport{
    HPACKIndexingPolicy: hpack.IndexingChrome,
    HPACKNeverIndex:     []string{"x-request-id"},
    HPACKAlwaysIndex:    []string{"accept"},
}
```

Available indexing policies:
- `IndexingDefault` - Standard Go behavior
- `IndexingChrome` - Emulates Chrome's indexing decisions
- `IndexingNever` - Never index any headers
- `IndexingAlways` - Always index headers (if they fit)

### Per-Request Header Ordering

Use magic keys to override header order per-request:

```go
req.Header[http2.HeaderOrderKey] = []string{"accept", "user-agent", "accept-language"}
req.Header[http2.PHeaderOrderKey] = []string{":method", ":authority", ":scheme", ":path"}
```

### PRIORITY Frames

Send PRIORITY frames after connection preface:

```go
transport := &http2.Transport{
    Priorities: []http2.Priority{
        {StreamID: 3, PriorityParam: http2.PriorityParam{Weight: 200, StreamDep: 0}},
        {StreamID: 5, PriorityParam: http2.PriorityParam{Weight: 100, StreamDep: 3}},
    },
}
```

## Chrome 133 Fingerprint

To match Chrome 133's HTTP/2 fingerprint:

```go
transport := &http2.Transport{
    Settings: map[http2.SettingID]uint32{
        http2.SettingHeaderTableSize:   65536,
        http2.SettingEnablePush:        0,
        http2.SettingInitialWindowSize: 6291456,
        http2.SettingMaxHeaderListSize: 262144,
    },
    SettingsOrder: []http2.SettingID{
        http2.SettingHeaderTableSize,
        http2.SettingEnablePush,
        http2.SettingInitialWindowSize,
        http2.SettingMaxHeaderListSize,
    },
    ConnectionFlow:     15663105,
    PseudoHeaderOrder:  []string{":method", ":authority", ":scheme", ":path"},
    HeaderPriority:     &http2.PriorityParam{Weight: 255, Exclusive: true, StreamDep: 0},
    StreamPriorityMode: http2.StreamPriorityChrome,
    HPACKIndexingPolicy: hpack.IndexingChrome,
}
```

Target Akamai hash: `52d84b11737d980aef856699f885ca86`

## API Reference

### Transport Fields

| Field | Type | Description |
|-------|------|-------------|
| `Settings` | `map[SettingID]uint32` | Custom HTTP/2 settings values |
| `SettingsOrder` | `[]SettingID` | Order to send settings in SETTINGS frame |
| `ConnectionFlow` | `uint32` | WINDOW_UPDATE value (default: 15663105) |
| `PseudoHeaderOrder` | `[]string` | Order of pseudo-headers |
| `HeaderOrder` | `[]string` | Order of regular headers |
| `HeaderPriority` | `*PriorityParam` | Priority data for HEADERS frame |
| `Priorities` | `[]Priority` | PRIORITY frames to send |
| `UserAgent` | `string` | Default User-Agent header |
| `StreamPriorityMode` | `StreamPriorityMode` | Stream priority calculation mode |
| `HPACKIndexingPolicy` | `hpack.IndexingPolicy` | HPACK dynamic table indexing |
| `HPACKNeverIndex` | `[]string` | Headers to never index |
| `HPACKAlwaysIndex` | `[]string` | Headers to always index |

### Stream Priority Modes

| Mode | Description |
|------|-------------|
| `StreamPriorityDefault` | Standard Go behavior |
| `StreamPriorityChrome` | Chrome's priority tree (exclusive on stream 0, weight 256) |
| `StreamPriorityFirefox` | Firefox's priority tree |
| `StreamPriorityCustom` | Use custom `StreamPriorityFunc` |

### HPACK Indexing Policies

| Policy | Description |
|--------|-------------|
| `IndexingDefault` | Standard Go behavior (index if fits) |
| `IndexingChrome` | Emulates Chrome's indexing decisions |
| `IndexingNever` | Never index any headers |
| `IndexingAlways` | Always index headers |

## Related Projects

- [sardanioss/http](https://github.com/sardanioss/http) - HTTP/1.1 fingerprinting (header order, keep-alive patterns)
- [refraction-networking/utls](https://github.com/refraction-networking/utls) - TLS fingerprinting

## License

BSD 3-Clause License (same as golang.org/x/net)
