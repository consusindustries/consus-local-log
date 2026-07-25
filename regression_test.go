// Regressions. Each test here pins a defect found in code review, so that the
// specific way the audit log could go wrong stays fixed.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestRequestCaptureIncompleteIsFlagged covers the most common real rejection:
// the gateway answers on headers alone (an expired key) and never reads the
// body. The capture is then only as far as the transport got, and the log must
// say so rather than presenting a half prompt as the whole one.
func TestRequestCaptureIncompleteIsFlagged(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // never touches r.Body
	}))
	defer upstream.Close()

	_, proxyURL, dir := startTestServer(t, upstream.URL, 64<<20, nil, "")

	payload := bytes.Repeat([]byte("a"), 8<<20)
	// The upload is cut short by the early response, so a client-side error
	// here is expected and not the thing under test.
	if resp, err := http.Post(proxyURL+"/v1/chat", "application/json", bytes.NewReader(payload)); err == nil {
		resp.Body.Close()
	}

	e := waitLines(t, dir, 1)[0]
	if !e.Truncated {
		t.Errorf("truncated = false for a body the upstream never read: %d of %d bytes captured, and the log claims that is all of it", len(e.Request), len(payload))
	}
	if len(e.Request) > len(payload) {
		t.Errorf("captured %d bytes, more than the %d sent", len(e.Request), len(payload))
	}
}

// TestBinaryResponseCapture covers gzip response bodies — what every stock SDK
// asks for. JSON string encoding would replace each invalid byte with U+FFFD,
// so the capture is base64-encoded and stays recoverable.
func TestBinaryResponseCapture(t *testing.T) {
	raw := gzipBytes(t, `{"id":"resp-1","choices":[]}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw)
	}))
	defer upstream.Close()

	_, proxyURL, dir := startTestServer(t, upstream.URL, 1<<20, nil, "")

	req, _ := http.NewRequest("GET", proxyURL+"/v1/chat", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req) // no client-side decompression
	if err != nil {
		t.Fatal(err)
	}
	relayed, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(relayed, raw) {
		t.Errorf("relayed %d bytes, want the %d upstream bytes unchanged", len(relayed), len(raw))
	}

	e := waitLines(t, dir, 1)[0]
	if !strings.HasPrefix(e.Response, binaryMarker) {
		t.Fatalf("response = %q, want a %s capture", e.Response, binaryMarker)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(e.Response, binaryMarker))
	if err != nil {
		t.Fatalf("logged capture does not decode: %v", err)
	}
	if !bytes.Equal(decoded, raw) {
		t.Errorf("decoded capture does not match the upstream bytes")
	}
}

// TestInvalidUpstreamStatus covers a status code a ResponseWriter refuses:
// relaying it verbatim panics the handler, which used to take the log entry
// down with it.
func TestInvalidUpstreamStatus(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				bufio.NewReader(c).ReadString('\n')
				io.WriteString(c, "HTTP/1.1 099 Weird\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhi")
			}()
		}
	}()

	_, proxyURL, dir := startTestServer(t, "http://"+l.Addr().String(), 1<<20, nil, "")

	resp, err := http.Get(proxyURL + "/v1/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Errorf("status = %d, want 502 for an unrelayable upstream code", resp.StatusCode)
	}

	e := waitLines(t, dir, 1)[0]
	if e.Status != 502 || e.Path != "/v1/x" {
		t.Errorf("log entry: %+v", e)
	}
}

// TestHealthzNeverBlocks covers the health endpoint answering instantly while
// the upstream is wedged — the moment an orchestrator's probe timing out would
// restart the proxy and sever every in-flight stream.
func TestHealthzNeverBlocks(t *testing.T) {
	s, proxyURL, _ := startTestServer(t, "http://"+blackholeListener(t), 1<<20, nil, "")
	startProber(t, s)

	for i := 0; i < 3; i++ {
		start := time.Now()
		resp, err := http.Get(proxyURL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		var health struct {
			UpstreamOK bool  `json:"upstream_ok"`
			UptimeS    int64 `json:"uptime_s"`
		}
		json.NewDecoder(resp.Body).Decode(&health)
		resp.Body.Close()
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("/healthz took %v with an unresponsive upstream, want an immediate answer", elapsed)
		}
		if health.UpstreamOK {
			t.Error("upstream_ok = true for an upstream that never answers")
		}
	}
}

// TestHealthzReportsReachableUpstream is the other half: the cached probe does
// turn true against a live upstream.
func TestHealthzReportsReachableUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	s, proxyURL, _ := startTestServer(t, upstream.URL, 1<<20, nil, "")
	startProber(t, s)

	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(proxyURL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		var health struct {
			UpstreamOK bool `json:"upstream_ok"`
		}
		json.NewDecoder(resp.Body).Decode(&health)
		resp.Body.Close()
		if health.UpstreamOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("upstream_ok never became true for a reachable upstream")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestHealthzIsNeverForwarded pins the local-only route.
func TestHealthzIsNeverForwarded(t *testing.T) {
	var forwarded atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Store(true)
	}))
	defer upstream.Close()

	_, proxyURL, _ := startTestServer(t, upstream.URL, 1<<20, nil, "")

	resp, err := http.Get(proxyURL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if forwarded.Load() {
		t.Error("/healthz reached the upstream")
	}
}

// TestPartialWriteRollback covers a log file that runs out of room mid-line.
// Without the rollback the file keeps a headless fragment, the next entry is
// appended straight onto it, and every jq recipe in the README dies on the
// malformed line — so a full disk would cost not just the dropped entry but
// the readability of the whole day's audit trail.
//
// It needs a filesystem small enough to fill, which cannot be arranged
// portably; CI mounts a 1 MB tmpfs and points LOCALLOG_TEST_SMALLDIR at it.
// See the README's Development section to run it locally.
func TestPartialWriteRollback(t *testing.T) {
	small := os.Getenv("LOCALLOG_TEST_SMALLDIR")
	if small == "" {
		t.Skip("set LOCALLOG_TEST_SMALLDIR to a small, writable filesystem to run this")
	}
	logDir, err := os.MkdirTemp(small, "locallog")
	if err != nil {
		t.Fatalf("cannot use %s: %v", small, err)
	}
	t.Cleanup(func() { os.RemoveAll(logDir) })

	u, _ := url.Parse("http://upstream.invalid")
	s := newServer(config{upstream: u, dir: logDir, maxCapture: 64 << 20})
	done := make(chan struct{})
	go s.runWriter(done)

	line := func(marker string, size int) entry {
		return entry{
			TS:      "2026-01-01T00:00:00.000Z",
			day:     "2026-01-01",
			Method:  "POST",
			Path:    "/v1/chat",
			Status:  200,
			Request: marker + strings.Repeat("a", size),
		}
	}
	s.logCh <- line("first-", 0)
	s.logCh <- line("huge-", 64<<20) // larger than the filesystem: cannot land
	s.logCh <- line("third-", 0)     // must still append cleanly afterwards
	close(s.logCh)
	<-done

	path := filepath.Join(logDir, "2026-01-01.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// parseLines fails the test on any line that does not parse; the suffix
	// check catches a fragment it would otherwise skip as a partial write.
	entries := parseLines(t, data, path)
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Errorf("log file ends mid-line: a partial write was left behind\ntail: %q", data[max(0, len(data)-80):])
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want the 2 that fit", len(entries))
	}
	for i, want := range []string{"first-", "third-"} {
		if !strings.HasPrefix(entries[i].Request, want) {
			t.Errorf("entry %d = %.16q, want the one starting %q", i, entries[i].Request, want)
		}
	}
	if s.misses.Load() != 1 {
		t.Errorf("log_misses = %d, want 1 for the entry that could not be written", s.misses.Load())
	}
	// The loss is also recorded durably: the line after the failure carries it.
	if entries[0].Dropped != 0 || entries[1].Dropped != 1 {
		t.Errorf("dropped = [%d, %d], want [0, 1] — the failed write must be reported by the next line", entries[0].Dropped, entries[1].Dropped)
	}
}

// completeThenVanish is a client that disappears the instant it holds the whole
// response — which is what every command-line client does when it exits.
type completeThenVanish struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (c *completeThenVanish) Write(p []byte) (int, error) {
	n, err := c.ResponseRecorder.Write(p)
	c.cancel() // the connection goes away as the last chunk lands
	return n, err
}

func (c *completeThenVanish) Flush() { c.ResponseRecorder.Flush() }

// TestNoDisconnectAfterCompleteResponse covers a client that hangs up the
// moment it has the full response. Recording that as client_disconnected
// misrepresents a perfectly normal request as a severed one, and a reviewer
// reading the audit trail would conclude the caller never got its answer.
//
// Found in production traffic: a preflight suite logged 8 of 101 requests this
// way, every one of them with a complete response body.
func TestNoDisconnectAfterCompleteResponse(t *testing.T) {
	const body = `{"id":"resp-1","ok":true}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	defer upstream.Close()

	s, _, dir := startTestServer(t, upstream.URL, 1<<20, nil, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &completeThenVanish{httptest.NewRecorder(), cancel}
	s.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat", nil).WithContext(ctx))

	if w.Code != 200 || w.Body.String() != body {
		t.Fatalf("client did not receive the whole response: %d %q", w.Code, w.Body.String())
	}
	e := waitLines(t, dir, 1)[0]
	if e.ClientDisconnected {
		t.Error("client_disconnected = true for a response the client received in full")
	}
	if e.Response != body {
		t.Errorf("response capture = %q, want %q", e.Response, body)
	}
}

// TestProbeNeverPoisonsTheConnectionPool covers the health prober sharing a
// transport with proxied traffic. A HEAD of an upstream whose base URL streams
// forever is complete for the client at the headers while the server side is
// still mid-handler; if that connection were pooled, the next real request to
// reuse it would wait forever for a response — and the proxy has no response
// timeout to break the wait, by design. Found as an intermittent suite hang:
// whether a request drew the poisoned connection or dialed fresh was a race.
func TestProbeNeverPoisonsTheConnectionPool(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			io.WriteString(w, "ok")
			return
		}
		// The base URL the prober HEADs: streams until the peer goes away.
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
				io.WriteString(w, "data: tick\n\n")
				fl.Flush()
			}
		}
	}))
	defer upstream.Close()

	s, proxyURL, dir := startTestServer(t, upstream.URL, 1<<20, nil, "")
	startProber(t, s)

	// Wait until a probe has completed, so any connection it leaked would now
	// be sitting in the idle pool.
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.probeMu.Lock()
		ok := s.upstreamOK
		s.probeMu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("prober never completed against the streaming upstream")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A real request through the proxy must not inherit the probe's
	// connection. The client timeout is the tripwire: with a poisoned pool
	// this Get waits forever for response headers.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(proxyURL + "/v1/x")
	if err != nil {
		t.Fatalf("request through the proxy hung after a probe: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("got %d %q, want 200 ok", resp.StatusCode, body)
	}
	if e := waitLines(t, dir, 1)[0]; e.Path != "/v1/x" || e.Status != 200 {
		t.Errorf("log entry: %+v", e)
	}
}
