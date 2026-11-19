// Shared helpers for the Go forge: deterministic time, request ids, hashing,
// base64url, and JSON replies.
//
// Determinism: nothing here reads a wall clock or an unseeded random source for
// state. The trial timestamp comes from MAKEFUN_TRIAL_NOW when the harness sets
// it, else the fixed scenario epoch (1735689600, 2025-01-01T00:00:00Z). Request
// ids are a per-process counter, so two runs with the same requests produce the
// same headers. The Date header is wall-clock (as in the Node server) but
// headers are not part of the state export, so it cannot affect determinism.

package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Fixed epoch (2025-01-01T00:00:00Z) used for every server-generated timestamp.
const fixedEpochSec = 1735689600

var trialNow = func() int64 {
	raw := os.Getenv("MAKEFUN_TRIAL_NOW")
	if raw != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && n > 0 {
			return n
		}
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw)); err == nil {
			return t.Unix()
		}
		if t, err := time.Parse("2006-01-02", strings.TrimSpace(raw)); err == nil {
			return t.Unix()
		}
		// Also accept a float epoch (e.g. "1735689600.5").
		if f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil && f > 0 {
			return int64(f)
		}
	}
	return fixedEpochSec
}()

// trialNowISO returns the trial "now" as an RFC 3339 UTC string.
func trialNowISO() string {
	return time.Unix(trialNow, 0).UTC().Format("2006-01-02T15:04:05Z")
}

// shopToken is the bearer token the suite presents (MAKEFUN_SHOP_TOKEN),
// read once at startup. Empty when unset (local dev).
var shopToken = os.Getenv("MAKEFUN_SHOP_TOKEN")

var requestCounter atomic.Uint64

// nextRequestID returns a non-empty, deterministic request id.
func nextRequestID() string {
	return "req-" + strconv.FormatUint(requestCounter.Add(1), 10)
}

// sha1Hex returns the lowercase hex SHA-1 of data.
func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

// b64url encodes text as unpadded base64url (opaque global ids).
func b64url(text string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(text))
}

// b64urlDecode decodes an unpadded base64url string.
func b64urlDecode(s string) (string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// decodeBase64 decodes a base64 (standard or url-safe, whitespace-tolerant)
// string, or returns nil. Node's decoder is lenient about padding, so pad to
// a multiple of 4 before retrying.
func decodeBase64(text string) []byte {
	cleaned := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, text)
	try := func(s string) []byte {
		if b, err := base64.StdEncoding.DecodeString(s); err == nil {
			return b
		}
		if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
			return b
		}
		if b, err := base64.URLEncoding.DecodeString(s); err == nil {
			return b
		}
		return nil
	}
	if b := try(cleaned); b != nil {
		return b
	}
	// Pad to a multiple of 4 and retry (Node accepts unpadded input).
	if m := len(cleaned) % 4; m != 0 {
		if b := try(cleaned + strings.Repeat("=", 4-m)); b != nil {
			return b
		}
	}
	return nil
}

// --- ordered JSON objects ---

// orderedMap is a JSON object whose keys marshal in insertion order (like a JS
// object literal), not sorted. GraphQL responses must preserve the selection
// order of the document, so the executor builds them with this type. Setting
// an existing key replaces its value in place (like JS Object.assign).
type orderedMap struct {
	keys []string
	vals map[string]any
}

func newOrderedMap() *orderedMap {
	return &orderedMap{vals: map[string]any{}}
}

func (o *orderedMap) set(key string, v any) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
}

func (o *orderedMap) get(key string) (any, bool) {
	v, ok := o.vals[key]
	return v, ok
}

func (o *orderedMap) MarshalJSON() ([]byte, error) {
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		sb.Write(kb)
		sb.WriteByte(':')
		vb, err := json.Marshal(o.vals[k])
		if err != nil {
			return nil, err
		}
		sb.Write(vb)
	}
	sb.WriteByte('}')
	return []byte(sb.String()), nil
}

// marshalJSON marshals v to JSON. It never fails for the shapes we emit;
// on error it returns a minimal error object.
func marshalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"message":"Internal Server Error"}`)
	}
	return b
}

// --- HTTP request/response shapes (shared by git, REST, GraphQL, MCP) ---

// Request is one parsed HTTP request as the handlers see it.
type Request struct {
	Method string
	Path   string // raw path (no query), split on '/'
	// RawQuery is the raw query string (no leading '?'). The parsed map is
	// built lazily by QueryMap: most requests have no query at all, and
	// building a map per request was pure overhead.
	RawQuery string
	Headers  map[string]string // lowercased names -> values
	Body     []byte
	// Oversized is set when the body exceeded the 64 MiB cap.
	Oversized bool

	queryParsed bool
	query       map[string][]string
}

// QueryMap returns the parsed query map, building it on first use.
func (r *Request) QueryMap() map[string][]string {
	if !r.queryParsed {
		r.query = parseQuery(r.RawQuery)
		r.queryParsed = true
	}
	return r.query
}

// Response is one reply: a status, ordered headers, and a body.
type Response struct {
	Status  int
	Headers [][2]string
	Body    []byte
}

const jsonType = "application/json; charset=utf-8"

// jsonResponse builds a JSON reply exactly like Node's sendJson: the body is
// JSON + a trailing newline, with content-type, content-length and
// x-github-request-id headers (then any extras, in order).
func jsonResponse(status int, v any, extra ...[2]string) Response {
	text := append(marshalJSON(v), '\n')
	h := [][2]string{
		{"content-type", jsonType},
		{"content-length", itoa(len(text))},
		{"x-github-request-id", nextRequestID()},
	}
	h = append(h, extra...)
	return Response{Status: status, Headers: h, Body: text}
}

// jsonError is a {"message": ...} JSON reply.
func jsonError(status int, message string) Response {
	return jsonResponse(status, map[string]string{"message": message})
}

// bytesResponse builds a raw-bytes reply exactly like Node's sendBytes:
// content-type + content-length, no request id.
func bytesResponse(status int, body []byte, contentType string) Response {
	return Response{
		Status: status,
		Headers: [][2]string{
			{"content-type", contentType},
			{"content-length", itoa(len(body))},
		},
		Body: body,
	}
}

// jsonBytesResponse is a bytes reply whose body is already JSON text (with its
// trailing newline), without a request id (used by the git surface's 404/413).
func jsonBytesResponse(status int, body []byte) Response {
	return bytesResponse(status, body, jsonType)
}

var notFoundJSON = []byte("{\"message\":\"Not Found\"}\n")
var unauthorizedJSON = []byte("{\"message\":\"Requires authentication\"}\n")

// isAuthorized checks the bearer token against MAKEFUN_SHOP_TOKEN. When the
// harness sets the token, the request must carry exactly it; when unset (local
// dev), any token passes.
func isAuthorized(headers map[string]string) bool {
	if shopToken == "" {
		return true
	}
	auth, ok := headers["authorization"]
	if !ok || !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	token := auth[len("Bearer "):]
	return token != "" && token == shopToken
}

// parseJSON parses a JSON body. Returns (value, true) or (nil, false).
// Numbers decode as float64, matching the Node JSON.parse behaviour the
// handlers rely on.
func parseJSON(raw []byte) (any, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	return v, true
}
