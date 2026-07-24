// Tests for consus-local-log come in three layers, one file each:
//
//   unit_test.go        the small pure pieces, in isolation
//   acceptance_test.go  the nine acceptance criteria from the spec — the contract
//   regression_test.go  defects found in review, each pinned by a test
//   e2e_test.go         the real binary, started and signalled as an operator would
//
// This file holds what they share. No test ever contacts api.consus.io: every
// upstream is an httptest stub, a raw socket, or a port with nothing on it.

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is the injectable clock for the day-roll test. Mutex-guarded so
// the writer goroutine and the test can use it under the race detector.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// safeBuf collects log output written by background goroutines.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogOutput redirects the standard logger for the duration of a test.
func captureLogOutput(t *testing.T) *safeBuf {
	t.Helper()
	var buf safeBuf
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// startTestServer builds a server from a config literal (no env vars), starts
// its writer goroutine, and serves it over a real listener. logDir == "" means
// a fresh temp dir. Cleanup order matters: the listener closes first so no
// handler can send on logCh after it is closed.
//
// The prober is left stopped: it would send HEAD requests of its own to the
// stub upstream, which for a stub that records what it saw is indistinguishable
// from the request under test. Tests that need it call startProber.
func startTestServer(t *testing.T, upstreamURL string, capBytes int64, clock func() time.Time, logDir string) (*server, string, string) {
	t.Helper()
	u, err := url.Parse(strings.TrimRight(upstreamURL, "/"))
	if err != nil {
		t.Fatalf("bad upstream URL %q: %v", upstreamURL, err)
	}
	if logDir == "" {
		logDir = t.TempDir()
	}
	s := newServer(config{listen: ":0", upstream: u, dir: logDir, maxCapture: capBytes})
	if clock != nil {
		s.now = clock
	}
	done := make(chan struct{})
	go s.runWriter(done)
	ts := httptest.NewServer(s)
	t.Cleanup(func() {
		ts.Close()
		close(s.logCh)
		<-done
	})
	return s, ts.URL, logDir
}

// startProber runs the background upstream health prober for the duration of a
// test.
func startProber(t *testing.T, s *server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.runProber(ctx)
}

// parseLines decodes every complete line in data. A trailing line without a
// newline is a write in progress and is skipped, not an error.
func parseLines(t *testing.T, data []byte, path string) []entry {
	t.Helper()
	lines := strings.Split(string(data), "\n")
	var out []entry
	for _, line := range lines[:len(lines)-1] {
		if line == "" {
			continue
		}
		var e entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("unparseable log line in %s: %v\nline: %s", path, err, line)
		}
		out = append(out, e)
	}
	return out
}

func readAllLines(t *testing.T, dir string) []entry {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []entry
	for _, fp := range files {
		data, err := os.ReadFile(fp)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, parseLines(t, data, fp)...)
	}
	return out
}

// waitLines polls the log dir until at least n entries have landed.
func waitLines(t *testing.T, dir string, n int) []entry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries := readAllLines(t, dir)
		if len(entries) >= n {
			return entries
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d log lines, have %d", n, len(entries))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitFileLines polls one specific log file until at least n entries land.
func waitFileLines(t *testing.T, path string, n int) []entry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			if entries := parseLines(t, data, path); len(entries) >= n {
				return entries
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d log lines in %s", n, path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// deadListener returns the address of a socket nothing is listening on.
func deadListener(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// blackholeListener accepts connections and never answers, the way a DROP
// firewall rule or a wedged upstream behaves.
func blackholeListener(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock() // hold a reference: a collected conn would be closed
			held = append(held, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		l.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			c.Close()
		}
	})
	return l.Addr().String()
}

func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := io.WriteString(zw, s); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
