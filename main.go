// Command consus-local-log is a stateless reverse proxy that customers run
// inside their own network boundary, between their tools and the Consus
// gateway. It forwards every request byte-for-byte (headers untouched,
// bodies never parsed), relays responses without added buffering so SSE
// streams in real time, and appends one JSON line per request to a local
// log file after the response completes.
//
// Design intent: a security engineer can read this entire file in one
// sitting and know exactly what it does — and, just as important, what it
// never does: it holds no credentials, validates nothing, modifies nothing,
// phones nowhere, and keeps no state beyond the log file it writes.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	// tsFormat is RFC 3339 with millisecond precision; entries are always UTC.
	tsFormat  = "2006-01-02T15:04:05.000Z07:00"
	dayFormat = "2006-01-02"

	probeInterval = 30 * time.Second
	probeTimeout  = 5 * time.Second

	upstreamUnreachable = "upstream unreachable"
)

// hopByHop is the fixed set of connection-level headers that must not be
// forwarded in either direction. Everything else is copied verbatim.
var hopByHop = []string{
	"Connection",
	"Keep-Alive",
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func isHopByHop(name string) bool {
	if len(name) >= 6 && strings.EqualFold(name[:6], "Proxy-") {
		return true
	}
	for _, h := range hopByHop {
		if strings.EqualFold(name, h) {
			return true
		}
	}
	return false
}

// modelRe finds a "model" JSON field with a plain string value. Best effort
// by design: the body is never parsed as JSON, and RE2's linear-time
// guarantee bounds the cost on the already-capped capture. Values containing
// escapes simply don't match and yield "".
var modelRe = regexp.MustCompile(`"model"\s*:\s*"([^"\\]*)"`)

func extractModel(body []byte) string {
	m := modelRe.FindSubmatch(body)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// binaryMarker prefixes a capture that had to be base64-encoded to survive
// JSON encoding.
const binaryMarker = "base64:"

// logString renders captured bytes for the log line. Text is stored verbatim,
// which is the normal case; anything that is not valid UTF-8 — a gzip-encoded
// response body, say — is base64-encoded instead, because encoding/json would
// otherwise replace every invalid byte with U+FFFD and the capture would be
// unrecoverable. Text that would be mistaken for an encoded capture is encoded
// too, so the marker never means anything else.
func logString(b []byte) string {
	if utf8.Valid(b) && !bytes.HasPrefix(b, []byte(binaryMarker)) {
		return string(b)
	}
	return binaryMarker + base64.StdEncoding.EncodeToString(b)
}

// keyHash is the only place the Authorization value is examined, and only to
// hash it for the log line. Forwarding never touches it: the header rides
// through copyHeaders with everything else.
func keyHash(auth string) string {
	if auth == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(auth))
	return hex.EncodeToString(sum[:])
}

type config struct {
	listen     string
	upstream   *url.URL
	dir        string
	maxCapture int64
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadConfig() (config, error) {
	cfg := config{
		listen: envOr("LOCALLOG_LISTEN", ":4000"),
		dir:    envOr("LOCALLOG_DIR", "/var/log/consus"),
	}
	// The trailing slash is trimmed so upstream+RequestURI never doubles one.
	raw := strings.TrimRight(envOr("LOCALLOG_UPSTREAM", "https://api.consus.io"), "/")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return config{}, fmt.Errorf("LOCALLOG_UPSTREAM %q is not an absolute URL", raw)
	}
	cfg.upstream = u
	rawCap := envOr("LOCALLOG_MAX_CAPTURE", "10485760")
	n, err := strconv.ParseInt(rawCap, 10, 64)
	if err != nil || n < 0 {
		return config{}, fmt.Errorf("LOCALLOG_MAX_CAPTURE %q is not a non-negative integer", rawCap)
	}
	cfg.maxCapture = n
	return cfg, nil
}

// entry is one log line. Every exported field is always present in the output.
type entry struct {
	TS                 string `json:"ts"`
	ConsusRequestID    string `json:"consus_request_id"`
	KeySHA256          string `json:"key_sha256"`
	Path               string `json:"path"`
	Method             string `json:"method"`
	Model              string `json:"model"`
	Status             int    `json:"status"`
	LatencyMS          int64  `json:"latency_ms"`
	Stream             bool   `json:"stream"`
	Truncated          bool   `json:"truncated"`
	ClientDisconnected bool   `json:"client_disconnected"`
	Request            string `json:"request"`
	Response           string `json:"response"`

	// day is the UTC date the request started, and decides which file the
	// entry belongs in. It is derived at request time rather than at write
	// time so a request that straddles midnight is filed under the day it
	// happened. Unexported, so it never appears in the log line.
	day string
}

// capBuf retains at most max bytes of whatever is written to it. Write never
// fails and never short-writes, so the traffic it observes is never affected;
// past the cap it stops retaining and marks the capture truncated.
//
// The mutex is load-bearing: the HTTP transport consumes the request body on
// its own goroutine, so the request capture is written and read on different
// goroutines.
type capBuf struct {
	mu        sync.Mutex
	max       int64
	buf       []byte
	truncated bool
}

func (c *capBuf) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - int64(len(c.buf)); room >= int64(len(p)) {
		c.buf = append(c.buf, p...)
	} else {
		if room > 0 {
			c.buf = append(c.buf, p[:room]...)
		}
		if len(p) > 0 {
			c.truncated = true
		}
	}
	return len(p), nil
}

// snapshot returns a copy of the retained bytes and whether the cap was hit.
func (c *capBuf) snapshot() ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]byte, len(c.buf))
	copy(out, c.buf)
	return out, c.truncated
}

// captureReader tees everything read from rc into dst. It is the request-side
// tee: the transport pulls from it while sending upstream, so the body streams
// to the upstream while being captured, never buffered in full.
type captureReader struct {
	rc  io.ReadCloser
	dst *capBuf
	// done reports that the body was read to its end. Until that happens the
	// capture is only as complete as the transport's progress so far.
	done atomic.Bool
}

func (t *captureReader) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		t.dst.Write(p[:n])
	}
	if err == io.EOF {
		t.done.Store(true)
	}
	return n, err
}

func (t *captureReader) Close() error { return t.rc.Close() }

// finish marks a request that has no body as fully captured.
func (t *captureReader) finish() { t.done.Store(true) }

// snapshot returns the captured bytes and whether the capture is incomplete:
// either the cap was hit, or the body was never read to its end because the
// upstream answered without reading it (a 401 on an expired key is the common
// case) or the client hung up mid-upload.
func (t *captureReader) snapshot() ([]byte, bool) {
	// Load done before reading the buffer, not after: if it is set, every
	// write that will ever happen has already happened, so what we read next
	// is the whole body. The other order could observe a complete flag for a
	// buffer that is still one chunk behind.
	complete := t.done.Load()
	body, capped := t.dst.snapshot()
	return body, capped || !complete
}

type server struct {
	cfg       config
	transport *http.Transport
	logCh     chan entry
	misses    atomic.Int64
	start     time.Time
	now       func() time.Time // seam for the day-roll test; always time.Now in production

	probeMu    sync.Mutex
	upstreamOK bool
}

func newServer(cfg config) *server {
	return &server{
		cfg: cfg,
		transport: &http.Transport{
			// DefaultTransport's connection-setup bounds, restated because a
			// custom Transport does not inherit them: without these, a
			// blackholed upstream stalls each request for minutes instead of
			// failing fast with a 502.
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true, // a custom DialContext otherwise disables HTTP/2
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			// A proxy must not renegotiate compression: without this, Go
			// would inject Accept-Encoding and transparently decompress,
			// altering the bytes the client and the log see.
			DisableCompression: true,
		},
		logCh: make(chan entry, 256),
		start: time.Now(),
		now:   time.Now,
	}
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		s.healthz(w)
		return
	}
	s.proxy(w, r)
}

// copyHeaders copies every header except the hop-by-hop set. Authorization is
// deliberately not special-cased anywhere in the forwarding path.
func copyHeaders(dst, src http.Header) {
	for name, values := range src {
		if isHopByHop(name) {
			continue
		}
		for _, v := range values {
			dst.Add(name, v)
		}
	}
}

// relayableStatus keeps the upstream's status code unless it falls outside the
// range a ResponseWriter accepts — an upstream sending one is not speaking
// HTTP we can relay, and passing it on would panic the handler.
func relayableStatus(code int) int {
	if code < 100 || code > 999 {
		return http.StatusBadGateway
	}
	return code
}

// proxy is the whole request lifecycle: capture-while-forwarding the request,
// relay-while-capturing the response, and exactly one log entry at the end.
func (s *server) proxy(w http.ResponseWriter, r *http.Request) {
	start := s.now()
	e := entry{
		TS:        start.UTC().Format(tsFormat),
		day:       start.UTC().Format(dayFormat),
		KeySHA256: keyHash(r.Header.Get("Authorization")),
		Path:      r.URL.RequestURI(),
		Method:    r.Method,
		Status:    http.StatusBadGateway, // until the upstream answers
	}
	reqTee := &captureReader{rc: r.Body, dst: &capBuf{max: s.cfg.maxCapture}}
	respCap := &capBuf{max: s.cfg.maxCapture}

	// The log line is written from a defer, so every exit from this function —
	// including a panic in the relay path — still leaves an audit record.
	defer func() {
		reqBody, reqIncomplete := reqTee.snapshot()
		respBody, respTruncated := respCap.snapshot()
		e.Model = extractModel(reqBody)
		e.Request = logString(reqBody)
		e.Response = logString(respBody)
		e.Truncated = reqIncomplete || respTruncated
		e.LatencyMS = s.now().Sub(start).Milliseconds()
		if r.Context().Err() != nil {
			e.ClientDisconnected = true
		}
		s.enqueue(e)
	}()

	var body io.Reader = reqTee
	if r.ContentLength == 0 {
		// For server requests ContentLength 0 means "no body" (-1 means
		// unknown). NoBody keeps bodiless methods bodiless instead of
		// switching them to chunked encoding.
		body = http.NoBody
		reqTee.finish()
	}

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, s.cfg.upstream.String()+r.URL.RequestURI(), body)
	if err != nil {
		// Unreachable for requests the listener already parsed; treated as an
		// upstream failure so the request is still answered and logged.
		http.Error(w, upstreamUnreachable, http.StatusBadGateway)
		return
	}
	outReq.ContentLength = r.ContentLength
	outReq.Host = s.cfg.upstream.Host
	copyHeaders(outReq.Header, r.Header)
	if _, ok := outReq.Header["User-Agent"]; !ok {
		// An empty value suppresses Go's default User-Agent; a proxy must not
		// add headers the client didn't send.
		outReq.Header.Set("User-Agent", "")
	}

	// RoundTrip, not http.Client: its defaults are exactly the spec — no
	// redirect following (3xx passes through as an ordinary response), no
	// cookie handling, no request mutation.
	resp, err := s.transport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, upstreamUnreachable, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	e.ConsusRequestID = resp.Header.Get("x-consus-request-id")
	e.Stream = strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	e.Status = relayableStatus(resp.StatusCode)

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(e.Status)

	// Relay loop: every chunk is captured, written to the client, and flushed
	// immediately so SSE events arrive as the upstream emits them.
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			respCap.Write(buf[:n])
			if _, werr := w.Write(buf[:n]); werr != nil {
				// Client went away: stop relaying. The deferred Body.Close
				// aborts the upstream read; nothing further is drained.
				e.ClientDisconnected = true
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}

// enqueue hands the entry to the writer goroutine without ever blocking the
// request. A full channel means the entry is dropped and counted as a miss.
func (s *server) enqueue(e entry) {
	select {
	case s.logCh <- e:
	default:
		s.misses.Add(1)
	}
}

func (s *server) healthz(w http.ResponseWriter) {
	s.probeMu.Lock()
	ok := s.upstreamOK
	s.probeMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		UpstreamOK bool  `json:"upstream_ok"`
		LogMisses  int64 `json:"log_misses"`
		UptimeS    int64 `json:"uptime_s"`
	}{ok, s.misses.Load(), int64(s.now().Sub(s.start).Seconds())})
}

// runProber refreshes the cached upstream health every probeInterval. It has
// its own goroutine so that /healthz answers instantly even while the upstream
// is unreachable — which is exactly when orchestrators are asking, and when a
// health check that blocked would get the proxy restarted mid-stream.
func (s *server) runProber(ctx context.Context) {
	for {
		ok := s.probe(ctx)
		s.probeMu.Lock()
		s.upstreamOK = ok
		s.probeMu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(probeInterval):
		}
	}
}

// probe checks upstream reachability. Any completed response counts as
// reachable, whatever its status code.
func (s *server) probe(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.cfg.upstream.String(), nil)
	if err != nil {
		return false
	}
	resp, err := s.transport.RoundTrip(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// runWriter is the only goroutine that touches the log file. It drains logCh,
// appending one JSON line per entry into the file named for the day that entry
// started. Any failure costs the entry, never the request — but it is reported
// on stderr, because an audit trail that silently stops is worse than one that
// complains. When logCh closes — after the listener has fully shut down — it
// finishes whatever is buffered, closes the file, and signals done.
func (s *server) runWriter(done chan<- struct{}) {
	defer close(done)
	var f *os.File
	var curDay string
	var size int64 // bytes known to be safely on disk in the current file
	var lastErr string
	defer func() {
		if f != nil {
			f.Close()
		}
	}()

	// miss records a lost entry, reporting each distinct failure once so a
	// misconfigured log directory is visible without flooding the journal.
	miss := func(err error) {
		s.misses.Add(1)
		if msg := err.Error(); msg != lastErr {
			lastErr = msg
			log.Printf("log write failed, entry dropped (requests are unaffected): %v", err)
		}
	}

	for e := range s.logCh {
		if f == nil || e.day != curDay {
			if f != nil {
				f.Close()
				f = nil
			}
			if err := os.MkdirAll(s.cfg.dir, 0o750); err != nil {
				miss(err)
				continue
			}
			nf, err := os.OpenFile(filepath.Join(s.cfg.dir, e.day+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
			if err != nil {
				miss(err)
				continue
			}
			fi, err := nf.Stat()
			if err != nil {
				nf.Close()
				miss(err)
				continue
			}
			f, curDay, size = nf, e.day, fi.Size()
		}
		line, err := json.Marshal(e)
		if err != nil {
			miss(err)
			continue
		}
		line = append(line, '\n')
		n, err := f.Write(line)
		if err != nil {
			// A short write — a full disk, typically — would leave a headless
			// fragment that the next entry gets appended onto, breaking every
			// consumer of the file. Roll the partial line back instead.
			if n > 0 {
				if terr := f.Truncate(size); terr != nil {
					log.Printf("could not roll back a partial log line, the file may have an unparseable line: %v", terr)
				}
			}
			miss(err)
			continue
		}
		size += int64(n)
	}
}

// checkLogDir reports whether entries will actually be able to land, so that a
// log directory the process cannot write is discovered at startup rather than
// at audit time.
func checkLogDir(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	probe := filepath.Join(dir, ".writable")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(probe)
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	s := newServer(cfg)
	done := make(chan struct{})
	go s.runWriter(done)

	proberCtx, stopProber := context.WithCancel(context.Background())
	go s.runProber(proberCtx)
	defer stopProber()

	// A logging failure must never stop the proxy from serving, so this warns
	// rather than exits — but it warns loudly, on the first line an operator
	// reads.
	if err := checkLogDir(cfg.dir); err != nil {
		log.Printf("WARNING: log directory %s is not writable, requests will be proxied but NOT logged: %v", cfg.dir, err)
	}

	// No timeouts anywhere: SSE responses stay open as long as the upstream
	// keeps them open.
	srv := &http.Server{Addr: cfg.listen, Handler: s}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.Printf("listening on %s, forwarding to %s, logging to %s", cfg.listen, cfg.upstream, cfg.dir)

	select {
	case err := <-errCh:
		log.Fatal(err)
	case <-ctx.Done():
	}

	// Restore the default signal disposition before the drain: shutdown waits
	// on in-flight streams with no deadline, and an operator who gives up and
	// sends a second SIGTERM must get a process that dies rather than one that
	// swallows the signal.
	stop()
	log.Print("shutting down, waiting for in-flight requests")

	// Graceful shutdown: stop accepting, wait for in-flight requests with no
	// deadline (streams may be long-lived; the spec forbids one under 60s).
	// Handlers enqueue as their last act and Shutdown returns only after all
	// handlers do, so closing the channel afterwards can never race a send.
	if err := srv.Shutdown(context.Background()); err != nil {
		log.Printf("shutdown: %v", err)
	}
	stopProber()
	close(s.logCh)
	<-done
}
