// main.go — entrypoint: bind the forge, register it with the harness env,
// and wait until the env says the suite is done.
//
// The harness runs a small env server next to this process and tells us where
// it lives through MAKEFUN_ENV_URL. The dance is four steps:
//
//   1. listen on a kernel-chosen (ephemeral) port — never 8080, which is the
//      env's own port on the same loopback;
//   2. POST {"port": <ours>} to <env>/register so the suite can reach us;
//   3. poll GET <env>/done until it answers {"done": true};
//   4. shut the server down and leave with code 0.
//
// When MAKEFUN_ENV_URL is not set there is no env to talk to, so the process
// leaves with code 1 at once. The forge itself lives in server.go; this file
// only runs the lifecycle. Go standard library only — no dependencies, no git
// binary, no network beyond the env.
//
// The env client is a minimal raw-TCP HTTP/1.1 client (no net/http): the env
// server is a plain local HTTP server, and a tiny client keeps the binary
// small and the behaviour explicit.

package main

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	envVar             = "MAKEFUN_ENV_URL"
	registerPath       = "/register"
	donePath           = "/done"
	registerFirstWait  = 100 * time.Millisecond
	registerMaxWait    = 2000 * time.Millisecond
	donePollInterval   = 250 * time.Millisecond
	envDialTimeout     = 5 * time.Second
	envResponseTimeout = 10 * time.Second
)

// envRequest performs one HTTP request against the env server and returns the
// status code and body. The connection is closed after each request.
func envRequest(method, rawURL, body string) (int, []byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	conn, err := net.DialTimeout("tcp", host, envDialTimeout)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(envResponseTimeout))

	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	var sb strings.Builder
	sb.WriteString(method + " " + path + " HTTP/1.1\r\n")
	sb.WriteString("host: " + u.Host + "\r\n")
	if body != "" {
		sb.WriteString("content-type: application/json\r\n")
		sb.WriteString("content-length: " + itoa(len(body)) + "\r\n")
	}
	sb.WriteString("connection: close\r\n\r\n")
	sb.WriteString(body)
	if _, err := conn.Write([]byte(sb.String())); err != nil {
		return 0, nil, err
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		return 0, nil, err
	}
	// Status line: "HTTP/1.1 200 OK".
	headEnd := -1
	for i := 0; i+3 < len(raw); i++ {
		if raw[i] == '\r' && raw[i+1] == '\n' && raw[i+2] == '\r' && raw[i+3] == '\n' {
			headEnd = i
			break
		}
	}
	if headEnd < 0 {
		return 0, nil, fmt.Errorf("malformed env response")
	}
	head := string(raw[:headEnd])
	firstLine := head
	if idx := strings.IndexByte(head, '\r'); idx >= 0 {
		firstLine = head[:idx]
	}
	fields := strings.SplitN(firstLine, " ", 3)
	if len(fields) < 2 {
		return 0, nil, fmt.Errorf("malformed env status line")
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, nil, err
	}
	return status, raw[headEnd+4:], nil
}

// postJSON posts a JSON body and reports the HTTP status, or -1 when the
// request fails outright.
func postJSON(rawURL, body string) int {
	status, _, err := envRequest("POST", rawURL, body)
	if err != nil {
		return -1
	}
	return status
}

// getJSON GETs a URL and parses its JSON body, or returns nil when the request
// or the parse fails.
func getJSON(rawURL string) map[string]any {
	status, body, err := envRequest("GET", rawURL, "")
	if err != nil || status < 200 || status >= 300 {
		return nil
	}
	v, ok := parseJSON(body)
	if !ok {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return m
}

// register tells the env which port we bound. The first register wins: a 409
// means another process already registered, and it does not move the base URL,
// so 409 counts as done. Anything else that is not a 2xx, or a request that
// fails outright, is retried with exponential backoff.
func register(envURL string, port int) {
	target := envURL + registerPath
	wait := registerFirstWait
	for {
		status := postJSON(target, `{"port":`+itoa(port)+`}`)
		if status >= 200 && status < 300 || status == 409 {
			return
		}
		time.Sleep(wait)
		wait *= 2
		if wait > registerMaxWait {
			wait = registerMaxWait
		}
	}
}

// waitForDone polls the env until it reports the suite finished. Each poll is
// a single short GET that answers at once; the request is never held open.
func waitForDone(envURL string) {
	target := envURL + donePath
	for {
		body := getJSON(target)
		if body != nil {
			if done, ok := body["done"].(bool); ok && done {
				return
			}
		}
		time.Sleep(donePollInterval)
	}
}

func main() {
	envURL := os.Getenv(envVar)
	if envURL == "" {
		fmt.Fprintf(os.Stderr, "cannot start: %s is not set\n", envVar)
		os.Exit(1)
	}
	server := NewServer(NewStore())
	port, err := server.Listen()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal error in the entrypoint:", err)
		os.Exit(1)
	}
	go server.Serve()
	register(envURL, port)
	waitForDone(envURL)
	server.Close()
	os.Exit(0)
}
