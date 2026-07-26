// The nine acceptance criteria from specifications/consus-local-log-SPEC.md.
//
// These are the contract between this program and the people who deploy it.
// They may be added to, but never weakened, skipped, or deleted.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Criterion 1: method, path, query, body, and all headers (including
// Authorization) arrive upstream byte-identical; hop-by-hop headers are
// stripped in both directions; Host is rewritten.
func TestPassthrough(t *testing.T) {
	var got struct {
		method, uri, body, host string
		header                  http.Header
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.method, got.uri, got.body, got.host = r.Method, r.URL.RequestURI(), string(b), r.Host
		got.header = r.Header.Clone()
		w.Header().Set("Keep-Alive", "timeout=5")
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(201)
		io.WriteString(w, "created")
	}))
	defer upstream.Close()

	s, _, dir := startTestServer(t, upstream.URL, 1<<20, nil, "")

	reqBody := `{"model":"fab-1","messages":[{"role":"user","content":"hi"}]}`
	// Direct ServeHTTP so hop-by-hop headers can be planted; a real client
	// would sanitize them before they ever reached the proxy.
	req := httptest.NewRequest("POST", "/v1/chat/completions?stream=true&x=1", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer fab-key-000")
	req.Header.Set("X-Custom", "custom-value")
	req.Header.Set("Content-Type", "application/json")
	for name, v := range map[string]string{
		"Connection": "keep-alive", "Keep-Alive": "timeout=5", "Te": "trailers",
		"Trailer": "X-T", "Transfer-Encoding": "identity", "Upgrade": "websocket",
		"Proxy-Authorization": "secret",
	} {
		req.Header.Set(name, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if got.method != "POST" || got.uri != "/v1/chat/completions?stream=true&x=1" {
		t.Errorf("upstream saw %s %s", got.method, got.uri)
	}
	if got.body != reqBody {
		t.Errorf("body not byte-identical:\n got: %q\nwant: %q", got.body, reqBody)
	}
	if a := got.header.Get("Authorization"); a != "Bearer fab-key-000" {
		t.Errorf("Authorization = %q, want it forwarded untouched", a)
	}
	if c := got.header.Get("X-Custom"); c != "custom-value" {
		t.Errorf("X-Custom = %q", c)
	}
	for _, name := range []string{"Connection", "Keep-Alive", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Proxy-Authorization"} {
		if v, ok := got.header[http.CanonicalHeaderKey(name)]; ok {
			t.Errorf("hop-by-hop header %s reached upstream: %v", name, v)
		}
	}
	// The proxy must not add headers the client never sent.
	for _, name := range []string{"User-Agent", "Accept-Encoding"} {
		if v, ok := got.header[name]; ok {
			t.Errorf("proxy injected %s: %v", name, v)
		}
	}
	wantHost, _ := url.Parse(upstream.URL)
	if got.host != wantHost.Host {
		t.Errorf("Host = %q, want %q", got.host, wantHost.Host)
	}

	if rec.Code != 201 || rec.Body.String() != "created" {
		t.Errorf("client got %d %q, want 201 created", rec.Code, rec.Body.String())
	}
	if v := rec.Header().Get("X-Upstream"); v != "yes" {
		t.Errorf("response header X-Upstream = %q, want yes", v)
	}
	if v := rec.Header().Get("Keep-Alive"); v != "" {
		t.Errorf("hop-by-hop response header Keep-Alive reached client: %q", v)
	}

	e := waitLines(t, dir, 1)[0]
	if e.Method != "POST" || e.Path != "/v1/chat/completions?stream=true&x=1" || e.Status != 201 {
		t.Errorf("log entry: %+v", e)
	}
	if e.KeySHA256 != sha256Hex("Bearer fab-key-000") {
		t.Errorf("key_sha256 = %q, want hash of raw Authorization value", e.KeySHA256)
	}
	if e.Model != "fab-1" || e.Request != reqBody || e.Response != "created" {
		t.Errorf("capture wrong: model=%q request=%q response=%q", e.Model, e.Request, e.Response)
	}
	if e.Stream || e.Truncated || e.ClientDisconnected {
		t.Errorf("flags wrong: %+v", e)
	}
	if _, err := time.Parse(time.RFC3339, e.TS); err != nil {
		t.Errorf("ts %q not RFC3339: %v", e.TS, err)
	}
}

// Criterion 2: each SSE event reaches the client before the upstream sends
// the next one (proves per-chunk flushing), and the log holds the verbatim
// transcript. The upstream waits for an ack after every event, so any
// buffering in the proxy stalls the exchange and fails the test — no sleeps,
// no timing sensitivity.
func TestSSEStreaming(t *testing.T) {
	const events = 10
	proceed := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < events; i++ {
			fmt.Fprintf(w, "data: event-%d\n\n", i)
			fl.Flush()
			select {
			case <-proceed:
			case <-time.After(5 * time.Second):
				return // client never saw the event; the test will report it
			}
		}
	}))
	defer upstream.Close()

	_, proxyURL, dir := startTestServer(t, upstream.URL, 1<<20, nil, "")

	resp, err := http.Get(proxyURL + "/v1/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	for i := 0; i < events; i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading event %d: %v (an event was not flushed before the next was sent)", i, err)
		}
		if want := fmt.Sprintf("data: event-%d\n", i); line != want {
			t.Fatalf("event %d = %q, want %q", i, line, want)
		}
		if blank, err := reader.ReadString('\n'); err != nil || blank != "\n" {
			t.Fatalf("event %d separator = %q, %v", i, blank, err)
		}
		select {
		case proceed <- struct{}{}:
		case <-time.After(5 * time.Second):
			t.Fatalf("upstream stopped waiting after event %d", i)
		}
	}
	if rest, _ := io.ReadAll(reader); len(rest) != 0 {
		t.Fatalf("unexpected trailing bytes: %q", rest)
	}

	var transcript strings.Builder
	for i := 0; i < events; i++ {
		fmt.Fprintf(&transcript, "data: event-%d\n\n", i)
	}
	e := waitLines(t, dir, 1)[0]
	if e.Response != transcript.String() {
		t.Errorf("log transcript:\n got: %q\nwant: %q", e.Response, transcript.String())
	}
	if !e.Stream {
		t.Error("stream = false, want true for text/event-stream")
	}
}

// Criterion 3: a 20 MB body against a 1 MB cap — upstream receives every
// byte, the log retains exactly the cap, truncated is set.
func TestTruncation(t *testing.T) {
	var upstreamGot atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		upstreamGot.Store(n)
	}))
	defer upstream.Close()

	_, proxyURL, dir := startTestServer(t, upstream.URL, 1<<20, nil, "")

	payload := bytes.Repeat([]byte("a"), 20<<20)
	resp, err := http.Post(proxyURL+"/v1/upload", "application/octet-stream", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := upstreamGot.Load(); got != 20<<20 {
		t.Errorf("upstream received %d bytes, want all %d", got, 20<<20)
	}

	e := waitLines(t, dir, 1)[0]
	if len(e.Request) != 1<<20 {
		t.Errorf("log retained %d bytes, want exactly the 1 MB cap", len(e.Request))
	}
	if !e.Truncated {
		t.Error("truncated = false, want true")
	}
}

// Criterion 4: 100 concurrent streaming requests produce exactly 100
// well-formed, non-interleaved lines. parseLines fails the test on any
// unparseable line; distinct markers prove no cross-request mixing.
func TestConcurrency(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", b)
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	_, proxyURL, dir := startTestServer(t, upstream.URL, 1<<20, nil, "")

	const n = 100
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"marker":"marker-%03d"}`, i)
			resp, err := http.Post(proxyURL+"/v1/echo", "application/json", strings.NewReader(body))
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	entries := waitLines(t, dir, n)
	if len(entries) != n {
		t.Fatalf("got %d log lines, want exactly %d", len(entries), n)
	}
	markers := make(map[string]bool)
	for _, e := range entries {
		markers[e.Request] = true
	}
	if len(markers) != n {
		t.Errorf("got %d distinct request captures, want %d (lines interleaved or duplicated)", len(markers), n)
	}
}

// Criterion 5: an unwritable log dir never fails a request; /healthz reports
// the misses, and the operator is told on stderr rather than left to discover
// an empty audit trail later.
func TestLogFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-bit test is not meaningful on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits are advisory")
	}
	logs := captureLogOutput(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o700) })

	_, proxyURL, _ := startTestServer(t, upstream.URL, 1<<20, nil, filepath.Join(parent, "logs"))

	resp, err := http.Get(proxyURL + "/v1/anything")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("request failed with %d; logging problems must never fail requests", resp.StatusCode)
	}

	// Both signals are asynchronous, and the counter is incremented just
	// before the message is written, so poll for each on its own.
	var misses int64
	var reported bool
	deadline := time.Now().Add(5 * time.Second)
	for !(misses > 0 && reported) {
		var health struct {
			LogMisses int64 `json:"log_misses"`
		}
		hr, err := http.Get(proxyURL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewDecoder(hr.Body).Decode(&health); err != nil {
			t.Fatal(err)
		}
		hr.Body.Close()
		misses = health.LogMisses
		reported = strings.Contains(logs.String(), "log write failed")
		if time.Now().After(deadline) {
			t.Fatalf("after 5s: log_misses = %d, stderr reported failure = %v; stderr:\n%s", misses, reported, logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Criterion 6: a client disconnect mid-stream still produces a log line, with
// client_disconnected set and the bytes captured so far.
func TestClientDisconnect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; ; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
				fmt.Fprintf(w, "data: tick-%d\n\n", i)
				fl.Flush()
			}
		}
	}))
	defer upstream.Close()

	_, proxyURL, dir := startTestServer(t, upstream.URL, 1<<20, nil, "")

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", proxyURL+"/v1/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel() // hang up mid-stream

	e := waitLines(t, dir, 1)[0]
	if !e.ClientDisconnected {
		t.Error("client_disconnected = false, want true")
	}
	if !strings.HasPrefix(e.Response, "data: tick-0") {
		t.Errorf("response capture = %q, want the bytes relayed before the disconnect", e.Response)
	}
}

// Criterion 7: the upstream's x-consus-request-id lands in the log line (and
// passes through to the client like any other header). x-consus-key-id — the
// attribution join key against the portal's API Keys page — is held to the
// same standard.
func TestRequestID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-consus-request-id", "rid-test-123")
		w.Header().Set("x-consus-key-id", "key-test-456")
		w.Header().Set("x-consus-key-label", "erics laptop — dev")
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	_, proxyURL, dir := startTestServer(t, upstream.URL, 1<<20, nil, "")

	resp, err := http.Get(proxyURL + "/v1/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if v := resp.Header.Get("x-consus-request-id"); v != "rid-test-123" {
		t.Errorf("client saw request id %q, want rid-test-123", v)
	}
	e := waitLines(t, dir, 1)[0]
	if e.ConsusRequestID != "rid-test-123" {
		t.Errorf("consus_request_id = %q, want rid-test-123", e.ConsusRequestID)
	}
	if e.ConsusKeyID != "key-test-456" {
		t.Errorf("consus_key_id = %q, want key-test-456", e.ConsusKeyID)
	}
	if e.ConsusKeyLabel != "erics laptop — dev" {
		t.Errorf("consus_key_label = %q, want the label verbatim", e.ConsusKeyLabel)
	}
}

// Criterion 8: entries written across a UTC date boundary land in two files.
func TestDayRoll(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	clk := &fakeClock{t: time.Date(2026, 1, 1, 23, 59, 59, 0, time.UTC)}
	s, _, dir := startTestServer(t, upstream.URL, 1<<20, clk.now, "")

	do := func() {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/a", nil))
		if rec.Code != 200 {
			t.Fatalf("status = %d", rec.Code)
		}
	}

	do()
	waitFileLines(t, filepath.Join(dir, "2026-01-01.jsonl"), 1)

	clk.set(time.Date(2026, 1, 2, 0, 0, 1, 0, time.UTC))
	do()
	waitFileLines(t, filepath.Join(dir, "2026-01-02.jsonl"), 1)

	if n := len(waitFileLines(t, filepath.Join(dir, "2026-01-01.jsonl"), 1)); n != 1 {
		t.Errorf("first day file has %d lines, want 1", n)
	}
}

// Criterion 9: upstream down — the client gets a one-line 502 and the log
// line is still written with status 502 and an empty response.
func TestUpstreamDown(t *testing.T) {
	_, proxyURL, dir := startTestServer(t, "http://"+deadListener(t), 1<<20, nil, "")

	resp, err := http.Get(proxyURL + "/v1/x")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if string(body) != upstreamUnreachable+"\n" {
		t.Errorf("body = %q, want the one-line 502 message", body)
	}

	e := waitLines(t, dir, 1)[0]
	if e.Status != 502 || e.Response != "" || e.ConsusRequestID != "" || e.ConsusKeyID != "" || e.ConsusKeyLabel != "" {
		t.Errorf("log entry: %+v", e)
	}
	if e.LatencyMS < 0 {
		t.Errorf("latency_ms = %d", e.LatencyMS)
	}
}
