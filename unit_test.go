// Unit tests: the capture buffers, the log-line encoding, header handling, the
// frozen log schema, and configuration. No sockets, no files except a temp dir.

package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCapBuf(t *testing.T) {
	cases := []struct {
		name      string
		max       int64
		writes    []string
		want      string
		truncated bool
	}{
		{"under cap", 8, []string{"abc", "de"}, "abcde", false},
		{"exactly at cap", 8, []string{"abcdefgh"}, "abcdefgh", false},
		{"over cap in one write", 8, []string{"abcdefghij"}, "abcdefgh", true},
		{"write after full", 8, []string{"abcdefgh", "x"}, "abcdefgh", true},
		{"zero cap", 0, []string{"a"}, "", true},
		{"empty write at cap", 1, []string{"a", ""}, "a", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &capBuf{max: tc.max}
			for _, w := range tc.writes {
				n, err := c.Write([]byte(w))
				if n != len(w) || err != nil {
					t.Fatalf("Write(%q) = (%d, %v), want (%d, nil): capture must never affect traffic", w, n, err, len(w))
				}
			}
			got, trunc := c.snapshot()
			if string(got) != tc.want || trunc != tc.truncated {
				t.Fatalf("snapshot() = (%q, %v), want (%q, %v)", got, trunc, tc.want, tc.truncated)
			}
		})
	}
}

func TestCapBufConcurrent(t *testing.T) {
	c := &capBuf{max: 1024}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.Write([]byte("0123456789abcdef"))
			}
		}()
	}
	wg.Wait()
	got, trunc := c.snapshot()
	if len(got) != 1024 || !trunc {
		t.Fatalf("after concurrent writes: len=%d truncated=%v, want 1024 true", len(got), trunc)
	}
}

// TestCaptureReaderIncomplete covers the flag that keeps a partially forwarded
// body from being logged as if it were whole.
func TestCaptureReaderIncomplete(t *testing.T) {
	t.Run("read to EOF is complete", func(t *testing.T) {
		tee := &captureReader{rc: io.NopCloser(strings.NewReader("hello")), dst: &capBuf{max: 1 << 10}}
		if _, err := io.ReadAll(tee); err != nil {
			t.Fatal(err)
		}
		body, incomplete := tee.snapshot()
		if string(body) != "hello" || incomplete {
			t.Fatalf("snapshot() = (%q, %v), want (\"hello\", false)", body, incomplete)
		}
	})

	t.Run("abandoned body is incomplete", func(t *testing.T) {
		tee := &captureReader{rc: io.NopCloser(strings.NewReader("hello world")), dst: &capBuf{max: 1 << 10}}
		io.CopyN(io.Discard, tee, 5) // upstream answered and stopped reading
		body, incomplete := tee.snapshot()
		if string(body) != "hello" || !incomplete {
			t.Fatalf("snapshot() = (%q, %v), want (\"hello\", true)", body, incomplete)
		}
	})

	t.Run("bodiless request is complete", func(t *testing.T) {
		tee := &captureReader{rc: http.NoBody, dst: &capBuf{max: 1 << 10}}
		tee.finish()
		body, incomplete := tee.snapshot()
		if len(body) != 0 || incomplete {
			t.Fatalf("snapshot() = (%q, %v), want (\"\", false)", body, incomplete)
		}
	})
}

func TestLogString(t *testing.T) {
	gzipped := gzipBytes(t, "hello compressed world")
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"empty", nil, ""},
		{"plain text", []byte(`{"model":"fab-1"}`), `{"model":"fab-1"}`},
		{"utf-8 text", []byte("héllo — ok"), "héllo — ok"},
		{"invalid utf-8", []byte{0xff, 0xfe, 0x00}, binaryMarker + base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe, 0x00})},
		{"gzip body", gzipped, binaryMarker + base64.StdEncoding.EncodeToString(gzipped)},
		// Text that looks like an encoded capture is encoded too, so the
		// marker is never ambiguous for a consumer.
		{"text starting with the marker", []byte("base64:hello"), binaryMarker + base64.StdEncoding.EncodeToString([]byte("base64:hello"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := logString(tc.in); got != tc.want {
				t.Fatalf("logString() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractModel(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"simple", `{"model":"fab-1","messages":[]}`, "fab-1"},
		{"whitespace", `{"model" :  "fab-2"}`, "fab-2"},
		{"absent", `{"messages":[]}`, ""},
		{"empty body", "", ""},
		{"non-string value", `{"model": 123}`, ""},
		{"empty string value", `{"model":""}`, ""},
		// Documented best-effort trade-offs of a raw search over unparsed bytes:
		{"nested key also matches", `{"metadata":{"model":"nested"}}`, "nested"},
		{"escaped value does not match", `{"model":"a\"b"}`, ""},
		{"not json at all", `model "model": "loose" text`, "loose"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractModel([]byte(tc.body)); got != tc.want {
				t.Fatalf("extractModel(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestIsHopByHop(t *testing.T) {
	for _, name := range []string{"Connection", "connection", "KEEP-ALIVE", "TE", "te", "Trailer", "Transfer-Encoding", "Upgrade", "Proxy-Authorization", "proxy-connection", "Proxy-Anything"} {
		if !isHopByHop(name) {
			t.Errorf("isHopByHop(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"Authorization", "Content-Type", "Host", "Approximate", "Prox", "Proximity", "X-Custom"} {
		if isHopByHop(name) {
			t.Errorf("isHopByHop(%q) = true, want false", name)
		}
	}
}

func TestCopyHeaders(t *testing.T) {
	src := http.Header{}
	for name, v := range map[string]string{
		"Connection":          "keep-alive",
		"Keep-Alive":          "timeout=5",
		"Te":                  "trailers",
		"Trailer":             "X-T",
		"Transfer-Encoding":   "chunked",
		"Upgrade":             "websocket",
		"Proxy-Authorization": "secret",
		"Proxy-Connection":    "keep-alive",
		"Authorization":       "Bearer fab-key-000",
		"Content-Type":        "application/json",
	} {
		src.Set(name, v)
	}
	src["X-Multi"] = []string{"a", "b"}

	dst := http.Header{}
	copyHeaders(dst, src)

	for _, name := range []string{"Connection", "Keep-Alive", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Proxy-Authorization", "Proxy-Connection"} {
		if _, ok := dst[name]; ok {
			t.Errorf("hop-by-hop header %s was copied", name)
		}
	}
	if got := dst.Get("Authorization"); got != "Bearer fab-key-000" {
		t.Errorf("Authorization = %q, want it copied verbatim", got)
	}
	if got := dst.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := dst["X-Multi"]; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("X-Multi = %v, want [a b] in order", got)
	}
}

func TestRelayableStatus(t *testing.T) {
	for in, want := range map[int]int{
		200: 200, 302: 302, 404: 404, 503: 503, 100: 100, 999: 999,
		// Codes a ResponseWriter would panic on become a plain 502.
		99: 502, 0: 502, -1: 502, 1000: 502,
	} {
		if got := relayableStatus(in); got != want {
			t.Errorf("relayableStatus(%d) = %d, want %d", in, got, want)
		}
	}
}

// TestEntryGoldenJSON freezes the log schema. Field names, order, and types
// are a public contract; if this test fails, the change breaks every consumer
// of the log files.
func TestEntryGoldenJSON(t *testing.T) {
	e := entry{
		TS:                 "2026-01-02T03:04:05.678Z",
		ConsusRequestID:    "rid-1",
		KeySHA256:          "deadbeef",
		Path:               "/v1/chat?stream=1",
		Method:             "POST",
		Model:              "fab-1",
		Status:             200,
		LatencyMS:          42,
		Stream:             true,
		Truncated:          false,
		ClientDisconnected: false,
		Request:            `{"model":"fab-1"}`,
		Response:           "data: ok\n\n",
		day:                "2026-01-02", // internal routing only, never serialized
	}
	got, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"ts":"2026-01-02T03:04:05.678Z","consus_request_id":"rid-1","key_sha256":"deadbeef","path":"/v1/chat?stream=1","method":"POST","model":"fab-1","status":200,"latency_ms":42,"stream":true,"truncated":false,"client_disconnected":false,"request":"{\"model\":\"fab-1\"}","response":"data: ok\n\n"}`
	if string(got) != want {
		t.Fatalf("log schema changed:\n got: %s\nwant: %s", got, want)
	}
}

func TestLoadConfig(t *testing.T) {
	clearEnv := func(t *testing.T) {
		for _, k := range []string{"LOCALLOG_LISTEN", "LOCALLOG_UPSTREAM", "LOCALLOG_DIR", "LOCALLOG_MAX_CAPTURE"} {
			t.Setenv(k, "")
		}
	}

	t.Run("defaults", func(t *testing.T) {
		clearEnv(t)
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.listen != ":4000" || cfg.dir != "/var/log/consus" || cfg.upstream.String() != "https://api.consus.io" || cfg.maxCapture != 10485760 {
			t.Fatalf("unexpected defaults: %+v", cfg)
		}
	})

	t.Run("overrides and trailing slash", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("LOCALLOG_LISTEN", "127.0.0.1:9999")
		t.Setenv("LOCALLOG_UPSTREAM", "http://gateway.internal:8443/")
		t.Setenv("LOCALLOG_DIR", "/tmp/locallog-test")
		t.Setenv("LOCALLOG_MAX_CAPTURE", "1024")
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.listen != "127.0.0.1:9999" || cfg.dir != "/tmp/locallog-test" || cfg.maxCapture != 1024 {
			t.Fatalf("unexpected config: %+v", cfg)
		}
		if cfg.upstream.String() != "http://gateway.internal:8443" {
			t.Fatalf("trailing slash not trimmed: %q", cfg.upstream)
		}
	})

	t.Run("bad values error", func(t *testing.T) {
		for _, tc := range [][2]string{
			{"LOCALLOG_MAX_CAPTURE", "abc"},
			{"LOCALLOG_MAX_CAPTURE", "-5"},
			{"LOCALLOG_UPSTREAM", "not a url"},
		} {
			clearEnv(t)
			t.Setenv(tc[0], tc[1])
			if _, err := loadConfig(); err == nil {
				t.Errorf("%s=%q: want error, got nil", tc[0], tc[1])
			}
		}
	})
}

// TestTransportBounds guards the connection-setup timeouts: without them a
// blackholed upstream hangs requests instead of failing fast.
func TestTransportBounds(t *testing.T) {
	u, _ := url.Parse("https://example.invalid")
	tr := newServer(config{upstream: u}).transport
	if tr.DialContext == nil {
		t.Error("DialContext is nil: dials are unbounded")
	}
	if tr.TLSHandshakeTimeout != 10*time.Second {
		t.Errorf("TLSHandshakeTimeout = %v, want 10s", tr.TLSHandshakeTimeout)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = false: a custom DialContext silently disables HTTP/2")
	}
	if !tr.DisableCompression {
		t.Error("DisableCompression = false: the proxy would renegotiate encoding")
	}
}

// TestWriterFilesByEntryDay covers the day the entry happened deciding its
// file, rather than whatever day the writer happens to be draining on.
func TestWriterFilesByEntryDay(t *testing.T) {
	dir := t.TempDir()
	u, _ := url.Parse("http://upstream.invalid")
	s := newServer(config{upstream: u, dir: dir, maxCapture: 1 << 20})
	done := make(chan struct{})
	go s.runWriter(done)

	// The third entry belongs to the earlier day: an SSE stream that opened
	// before midnight can finish, and be logged, well after it.
	s.logCh <- entry{TS: "2026-01-01T23:59:58.000Z", Method: "GET", day: "2026-01-01"}
	s.logCh <- entry{TS: "2026-01-02T00:00:01.000Z", Method: "GET", day: "2026-01-02"}
	s.logCh <- entry{TS: "2026-01-01T23:59:59.000Z", Method: "GET", day: "2026-01-01"}
	close(s.logCh)
	<-done

	for _, tc := range []struct {
		file string
		want int
	}{{"2026-01-01.jsonl", 2}, {"2026-01-02.jsonl", 1}} {
		data, err := os.ReadFile(filepath.Join(dir, tc.file))
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		if got := len(parseLines(t, data, tc.file)); got != tc.want {
			t.Errorf("%s has %d lines, want %d", tc.file, got, tc.want)
		}
	}
	if misses := s.misses.Load(); misses != 0 {
		t.Errorf("log_misses = %d, want 0", misses)
	}
}
