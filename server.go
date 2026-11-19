// The forge's HTTP server: one in-memory store, four surfaces (see dispatch).
// A raw TCP HTTP/1.1 server in Go (a port of the Node rawhttp.ts prototype):
// one goroutine per connection, buffered request parsing, sequential dispatch per
// connection, keep-alive, a single pre-built response buffer per reply (ONE Write
// syscall), a Date header cached per second, a 30 s idle timeout, TCP_NODELAY.
//
// The raw server is the fast path: no net/http middleware, no per-request
// allocations beyond the response buffer itself. The store mutex is held only
// for the dispatch (handler) duration, never across a socket write.

package main

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxRequestHeadBytes = 64 * 1024
	maxBodyBytes        = 64 * 1024 * 1024
	idleTimeout         = 30 * time.Second
)

// ------------------------------------------------------------------ Date header

// dateEntry is one cached Date header value. The whole entry is swapped
// atomically, so readers never need a lock (the old mutex version showed up
// in profiles as a per-request lock/unlock pair).
type dateEntry struct {
	sec  int64
	text string
}

var dateCacheVal atomic.Value // dateEntry

// dateHeader returns the current HTTP Date, cached per second.
func dateHeader() string {
	now := time.Now().UTC()
	sec := now.Unix()
	if e, ok := dateCacheVal.Load().(dateEntry); ok && e.sec == sec {
		return e.text
	}
	e := dateEntry{sec: sec, text: now.Format("Mon, 02 Jan 2006 15:04:05 GMT")}
	dateCacheVal.Store(e)
	return e.text
}

var statusLines = map[int]string{
	200: "HTTP/1.1 200 OK\r\n",
	201: "HTTP/1.1 201 Created\r\n",
	202: "HTTP/1.1 202 Accepted\r\n",
	204: "HTTP/1.1 204 No Content\r\n",
	400: "HTTP/1.1 400 Bad Request\r\n",
	401: "HTTP/1.1 401 Unauthorized\r\n",
	404: "HTTP/1.1 404 Not Found\r\n",
	405: "HTTP/1.1 405 Method Not Allowed\r\n",
	409: "HTTP/1.1 409 Conflict\r\n",
	413: "HTTP/1.1 413 Content Too Large\r\n",
	500: "HTTP/1.1 500 Internal Server Error\r\n",
}

func statusLine(status int) string {
	if s, ok := statusLines[status]; ok {
		return s
	}
	return "HTTP/1.1 " + itoa(status) + " " + reasonPhrase(status) + "\r\n"
}

func reasonPhrase(status int) string {
	switch status {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 202:
		return "Accepted"
	case 204:
		return "No Content"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 413:
		return "Content Too Large"
	case 500:
		return "Internal Server Error"
	default:
		return "OK"
	}
}

// ------------------------------------------------------------------ dispatch

// dispatch routes one request through the four surfaces. It never throws to
// the caller: a handler panic is answered with a JSON 500 and the server stays
// up (the suite's repo-survives-errors check).
func dispatch(store *Store, req *Request) (resp Response) {
	defer func() {
		if r := recover(); r != nil {
			resp = jsonResponse(500, map[string]string{"message": fmt.Sprintf("%v", r)})
		}
	}()
	path := req.Path
	method := req.Method
	segments := splitSegments(path)

	// The env's boot check: 200 {"ok":true}, no auth.
	if method == "GET" && path == "/healthz" {
		return jsonResponse(200, map[string]bool{"ok": true})
	}

	// GraphQL.
	if path == "/api/graphql" {
		if r, ok := handleGraphQL(store, req); ok {
			return r
		}
		return jsonResponse(404, map[string]string{"message": "Not Found"})
	}

	// MCP.
	if path == "/mcp" {
		if r, ok := handleMCP(store, req); ok {
			return r
		}
		return jsonResponse(404, map[string]string{"message": "Not Found"})
	}

	// REST v3 (the /api/v3 root is segments.length === 0 after the prefix).
	if len(segments) >= 2 && segments[0] == "api" && segments[1] == "v3" {
		if r, ok := handleRest(store, req, segments[2:]); ok {
			return r
		}
		return jsonResponse(404, map[string]string{"message": "Not Found"})
	}

	// Git smart HTTP: /{owner}/{repo}.git/...
	if len(segments) >= 2 && strings.HasSuffix(segments[1], ".git") {
		if r, ok := handleGit(store, req, segments); ok {
			return r
		}
		return jsonResponse(404, map[string]string{"message": "Not Found"})
	}

	return jsonResponse(404, map[string]string{"message": "Not Found"})
}

// splitSegments splits a path on '/' and drops empty pieces.
func splitSegments(path string) []string {
	parts := strings.Split(path, "/")
	out := parts[:0]
	for _, s := range parts {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// parseQuery parses a URL query string into a map of key -> values,
// percent-decoding keys and values (+ becomes a space, like URLSearchParams).
func parseQuery(raw string) map[string][]string {
	out := map[string][]string{}
	if raw == "" {
		return out
	}
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		key, value := pair, ""
		if i := strings.IndexByte(pair, '='); i >= 0 {
			key, value = pair[:i], pair[i+1:]
		}
		if k, err := url.QueryUnescape(key); err == nil {
			key = k
		}
		if v, err := url.QueryUnescape(value); err == nil {
			value = v
		}
		out[key] = append(out[key], value)
	}
	return out
}

// splitTarget splits a request target into (path, query). Absolute-form
// targets (http://host/path?query) are reduced to origin-form.
func splitTarget(target string) (string, string) {
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		if u, err := url.Parse(target); err == nil {
			path := u.EscapedPath()
			if path == "" {
				path = "/"
			}
			return path, u.RawQuery
		}
	}
	path, query := target, ""
	if i := strings.IndexByte(target, '?'); i >= 0 {
		path, query = target[:i], target[i+1:]
	}
	if path == "" {
		path = "/"
	}
	return path, query
}

// ------------------------------------------------------------------ server

type Server struct {
	store    *Store
	listener net.Listener
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	closed   bool
}

func NewServer(store *Store) *Server {
	return &Server{store: store, conns: map[net.Conn]struct{}{}}
}

// Listen binds an ephemeral port and returns it.
func (s *Server) Listen() (int, error) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}
	s.listener = ln
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("the server did not bind to a TCP port")
	}
	return addr.Port, nil
}

// Serve accepts connections until Close is called.
func (s *Server) Serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			tcp.SetNoDelay(true)
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			conn.Close()
			return
		}
		s.conns[conn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go s.handleConn(conn)
	}
}

// Close stops accepting, closes every connection, and waits for handlers.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	if s.listener != nil {
		s.listener.Close()
	}
	for conn := range s.conns {
		conn.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) removeConn(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
	s.wg.Done()
}

// writeResponse serializes one reply into a single buffer and writes it once.
// Built with appends only (no string concatenation, no []byte(string)
// conversions) on a pre-sized buffer.
func writeResponse(conn net.Conn, resp Response, keepAlive bool) error {
	isNoBody := resp.Status == 204 || resp.Status == 304
	buf := make([]byte, 0, len(resp.Body)+256)
	buf = append(buf, statusLine(resp.Status)...)
	hasContentLength := false
	for _, h := range resp.Headers {
		if isNoBody && strings.EqualFold(h[0], "content-length") {
			continue
		}
		if strings.EqualFold(h[0], "content-length") {
			hasContentLength = true
		}
		buf = append(buf, h[0]...)
		buf = append(buf, ": "...)
		buf = append(buf, h[1]...)
		buf = append(buf, "\r\n"...)
	}
	if !isNoBody && !hasContentLength {
		buf = append(buf, "content-length: "...)
		buf = strconv.AppendInt(buf, int64(len(resp.Body)), 10)
		buf = append(buf, "\r\n"...)
	}
	buf = append(buf, "date: "...)
	buf = append(buf, dateHeader()...)
	buf = append(buf, "\r\n"...)
	if keepAlive {
		buf = append(buf, "connection: keep-alive\r\nkeep-alive: timeout=30\r\n"...)
	} else {
		buf = append(buf, "connection: close\r\n"...)
	}
	buf = append(buf, "\r\n"...)
	if !isNoBody {
		buf = append(buf, resp.Body...)
	}
	_, err := conn.Write(buf)
	return err
}

// writeSimple writes a minimal error reply (400/413) and reports whether the
// connection should be closed afterwards.
func writeSimple(conn net.Conn, status int, message string, keepAlive bool) error {
	body := []byte("{\"message\":\"" + message + "\"}\n")
	resp := Response{
		Status: status,
		Headers: [][2]string{
			{"content-type", jsonType},
			{"content-length", itoa(len(body))},
			{"x-github-request-id", nextRequestID()},
		},
		Body: body,
	}
	return writeResponse(conn, resp, keepAlive)
}

// handleConn serves one connection until it closes, errors, or goes idle.
func (s *Server) handleConn(conn net.Conn) {
	defer s.removeConn(conn)
	defer conn.Close()

	buf := make([]byte, 0, 65536)
	tmp := make([]byte, 65536)

	for {
		// Find a complete request head in the buffered bytes.
		headEnd := -1
		for {
			if idx := indexCRLFCRLF(buf); idx >= 0 {
				headEnd = idx
				break
			}
			if len(buf) > maxRequestHeadBytes {
				writeSimple(conn, 400, "Bad Request", false)
				return
			}
			conn.SetReadDeadline(time.Now().Add(idleTimeout))
			n, err := conn.Read(tmp)
			if err != nil {
				return
			}
			buf = append(buf, tmp[:n]...)
		}

		head := buf[:headEnd]

		// Parse the request line + headers directly on the head bytes:
		// no string(head) copy, no Split, no per-line allocations. Only the
		// method/target strings and the header map entries allocate, and
		// header names/values skip ToLower/TrimSpace when they are already
		// in canonical form (the common case for the suite's client).
		rlEnd := -1
		for i := 0; i+1 < len(head); i++ {
			if head[i] == '\r' && head[i+1] == '\n' {
				rlEnd = i
				break
			}
		}
		if rlEnd < 0 {
			writeSimple(conn, 400, "Bad Request", false)
			return
		}
		reqLine := head[:rlEnd]
		sp1 := bytes.IndexByte(reqLine, ' ')
		if sp1 <= 0 {
			writeSimple(conn, 400, "Bad Request", false)
			return
		}
		sp2 := bytes.IndexByte(reqLine[sp1+1:], ' ')
		if sp2 < 0 {
			writeSimple(conn, 400, "Bad Request", false)
			return
		}
		sp2 += sp1 + 1
		method := string(reqLine[:sp1])
		target := string(reqLine[sp1+1 : sp2])
		version := reqLine[sp2+1:]
		if !bytes.Equal(version, []byte("HTTP/1.0")) && !bytes.Equal(version, []byte("HTTP/1.1")) && !bytes.Equal(version, []byte("HTTP/2.0")) {
			writeSimple(conn, 400, "Bad Request", false)
			return
		}
		if !strings.HasPrefix(target, "/") && target != "*" {
			writeSimple(conn, 400, "Bad Request", false)
			return
		}
		headers := make(map[string]string, 8)
		badHead := false
		pos := rlEnd + 2
		for pos < len(head) {
			eol := -1
			for i := pos; i+1 < len(head); i++ {
				if head[i] == '\r' && head[i+1] == '\n' {
					eol = i
					break
				}
			}
			var line []byte
			if eol < 0 {
				line = head[pos:]
				pos = len(head)
			} else {
				line = head[pos:eol]
				pos = eol + 2
			}
			if len(line) == 0 {
				continue
			}
			colon := bytes.IndexByte(line, ':')
			if colon <= 0 {
				badHead = true
				break
			}
			name := headerName(line[:colon])
			value := headerValue(line[colon+1:])
			if prev, ok := headers[name]; ok {
				headers[name] = prev + ", " + value
			} else {
				headers[name] = value
			}
		}
		if badHead {
			writeSimple(conn, 400, "Bad Request", false)
			return
		}

		// Keep-alive: HTTP/1.1 defaults to keep-alive unless `connection: close`;
		// HTTP/1.0 defaults to close unless `connection: keep-alive`.
		keepAlive := !bytes.Equal(version, []byte("HTTP/1.0"))
		if connHdr, ok := headers["connection"]; ok {
			lower := strings.ToLower(connHdr)
			if strings.Contains(lower, "close") {
				keepAlive = false
			} else if strings.Contains(lower, "keep-alive") {
				keepAlive = true
			}
		}

		// Read the body (content-length or chunked), honouring Expect: 100-continue.
		// All newly-read bytes go into `buf` so the consumed-byte accounting
		// stays exact for pipelined requests.
		path, query := splitTarget(target)
		req := &Request{
			Method:   method,
			Path:     path,
			RawQuery: query,
			Headers:  headers,
		}
		expectContinue := false
		if expect, ok := headers["expect"]; ok && strings.EqualFold(strings.TrimSpace(expect), "100-continue") {
			expectContinue = true
		}
		chunked := false
		if te, ok := headers["transfer-encoding"]; ok && strings.Contains(strings.ToLower(te), "chunked") {
			chunked = true
		}
		var contentLength int64 = -1
		if cl, ok := headers["content-length"]; ok && !chunked {
			if n, err := strconv.ParseInt(strings.TrimSpace(strings.Split(cl, ",")[0]), 10, 64); err == nil && n >= 0 {
				contentLength = n
			}
		}
		if contentLength > maxBodyBytes {
			req.Oversized = true
			writeSimple(conn, 413, "Request body too large", false)
			return
		}

		bodyStart := headEnd + 4
		consumed := 0
		if chunked {
			for {
				decoded, used, oversized, ok := decodeChunked(buf[bodyStart:], maxBodyBytes)
				if oversized {
					writeSimple(conn, 413, "Request body too large", false)
					return
				}
				if ok {
					req.Body = decoded
					consumed = bodyStart + used
					break
				}
				if expectContinue {
					conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
					expectContinue = false
				}
				conn.SetReadDeadline(time.Now().Add(idleTimeout))
				n, err := conn.Read(tmp)
				if err != nil {
					return
				}
				buf = append(buf, tmp[:n]...)
			}
		} else if contentLength >= 0 {
			for int64(len(buf)-bodyStart) < contentLength {
				if expectContinue {
					conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
					expectContinue = false
				}
				conn.SetReadDeadline(time.Now().Add(idleTimeout))
				n, err := conn.Read(tmp)
				if err != nil {
					return
				}
				buf = append(buf, tmp[:n]...)
				if int64(len(buf)-bodyStart) > maxBodyBytes {
					writeSimple(conn, 413, "Request body too large", false)
					return
				}
			}
			req.Body = buf[bodyStart : bodyStart+int(contentLength)]
			consumed = bodyStart + int(contentLength)
		} else {
			req.Body = []byte{}
			consumed = bodyStart
		}

		// Dispatch (the store mutex is held for the handler, never across I/O).
		s.store.Lock()
		resp := dispatch(s.store, req)
		s.store.Unlock()

		if err := writeResponse(conn, resp, keepAlive); err != nil {
			return
		}
		if !keepAlive {
			return
		}
		// Keep any pipelined bytes for the next request.
		buf = append(buf[:0], buf[consumed:]...)
	}
}

// headerName lowercases + trims a header name exactly like
// strings.ToLower(strings.TrimSpace(string(b))) did, but without allocating
// when the name is already lowercase ASCII with no surrounding spaces (the
// common case). Any byte that could change the result (uppercase, non-ASCII,
// surrounding space) falls back to the exact old expression.
func headerName(b []byte) string {
	for _, c := range b {
		if c >= 'A' && c <= 'Z' || c >= 0x80 || isSpaceByte(c) {
			return strings.ToLower(strings.TrimSpace(string(b)))
		}
	}
	return string(b)
}

// isSpaceByte reports whether b is an ASCII whitespace byte that
// strings.TrimSpace would trim at a string edge.
func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\v' || b == '\f' || b == '\r'
}

// headerValue trims a header value exactly like strings.TrimSpace(string(b))
// did, but without allocating when neither edge needs trimming.
func headerValue(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if b[0] >= 0x80 || b[len(b)-1] >= 0x80 || isSpaceByte(b[0]) || isSpaceByte(b[len(b)-1]) {
		return strings.TrimSpace(string(b))
	}
	return string(b)
}

// indexCRLFCRLF returns the index of the first "\r\n\r\n" in b, or -1.
func indexCRLFCRLF(b []byte) int {
	for i := 0; i+3 < len(b); i++ {
		if b[i] == '\r' && b[i+1] == '\n' && b[i+2] == '\r' && b[i+3] == '\n' {
			return i
		}
	}
	return -1
}

// decodeChunked decodes a chunked transfer-encoding body. Returns (decoded,
// bytesConsumed, oversized, complete). complete is false when more bytes are
// needed; oversized is true when the body exceeds maxBytes.
func decodeChunked(body []byte, maxBytes int64) ([]byte, int, bool, bool) {
	var out []byte
	pos := 0
	for {
		// Chunk-size line.
		lineEnd := -1
		for i := pos; i+1 < len(body); i++ {
			if body[i] == '\r' && body[i+1] == '\n' {
				lineEnd = i
				break
			}
		}
		if lineEnd < 0 {
			return nil, 0, false, false
		}
		sizeLine := string(body[pos:lineEnd])
		if semi := strings.IndexByte(sizeLine, ';'); semi >= 0 {
			sizeLine = sizeLine[:semi]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(sizeLine), 16, 64)
		if err != nil || size < 0 {
			return nil, 0, false, true // malformed: treat as oversized/close
		}
		pos = lineEnd + 2
		if size == 0 {
			// Trailers until an empty line.
			for {
				end := -1
				for i := pos; i+1 < len(body); i++ {
					if body[i] == '\r' && body[i+1] == '\n' {
						end = i
						break
					}
				}
				if end < 0 {
					return nil, 0, false, false
				}
				if end == pos {
					return out, pos + 2, false, true
				}
				pos = end + 2
			}
		}
		if pos+int(size)+2 > len(body) {
			return nil, 0, false, false
		}
		if int64(len(out))+size > maxBytes {
			return nil, 0, true, false
		}
		out = append(out, body[pos:pos+int(size)]...)
		pos += int(size) + 2 // data + CRLF
	}
}
