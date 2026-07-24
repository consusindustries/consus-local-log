// End-to-end tests against the real binary: build it, run it, drive it over a
// socket, signal it. This is the only layer that exercises main() — startup
// checks, signal handling, and the drain that must not lose buffered entries.
//
// Each test builds the binary, so they are skipped under -short.

package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// requireBinaryTest skips the tests that need to compile and signal a process.
func requireBinaryTest(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping binary build in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM cannot be delivered to a subprocess on Windows")
	}
}

// buildBinary compiles the program exactly as a customer would.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "consus-local-log")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// startBinary runs the program with the given environment and waits for it to
// answer /healthz. The caller owns the process from there — nothing here reaps
// it, because each test ends it in its own way.
func startBinary(t *testing.T, bin, addr, upstream, logDir string) (*exec.Cmd, *safeBuf) {
	t.Helper()
	var stderr safeBuf
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"LOCALLOG_LISTEN="+addr,
		"LOCALLOG_UPSTREAM="+upstream,
		"LOCALLOG_DIR="+logDir,
	)
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
			resp.Body.Close()
			return cmd, &stderr
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("binary never became ready\nstderr: %s", stderr.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// endlessStream is an upstream that keeps a response open until the client goes
// away, so a shutdown has something real to wait on.
func endlessStream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; ; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
				fmt.Fprintf(w, "data: tick-%d\n\n", i)
				fl.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestE2EGracefulShutdown builds and runs the actual program, exercises it
// over real sockets with real env-var config, then SIGTERMs it: signal
// handling, the drain of the log channel, and exit 0 on graceful shutdown.
func TestE2EGracefulShutdown(t *testing.T) {
	requireBinaryTest(t)
	bin := buildBinary(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	addr := deadListener(t)
	logDir := t.TempDir()
	cmd, stderr := startBinary(t, bin, addr, upstream.URL, logDir)

	req, _ := http.NewRequest("POST", "http://"+addr+"/v1/chat", strings.NewReader(`{"model":"fab-1"}`))
	req.Header.Set("Authorization", "Bearer fab-key-e2e")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("exit status: %v (want 0)\nstderr: %s", err, stderr.String())
	}

	entries := readAllLines(t, logDir)
	if len(entries) != 1 {
		t.Fatalf("got %d log lines after shutdown, want 1 (drain failed?)\nstderr: %s", len(entries), stderr.String())
	}
	e := entries[0]
	if e.Status != 200 || e.Model != "fab-1" || e.KeySHA256 != sha256Hex("Bearer fab-key-e2e") {
		t.Errorf("log entry: %+v", e)
	}
}

// TestE2ESecondSignalTerminates covers the escape hatch from a drain that has
// no deadline. The first signal starts the graceful shutdown, which waits on
// the open stream; a second one has to end the process. If the handler were
// left installed for the whole drain it would swallow that signal, and an
// operator restarting the service would watch it hang until the service
// manager SIGKILLed it — discarding the buffered entries the drain exists to
// save.
func TestE2ESecondSignalTerminates(t *testing.T) {
	requireBinaryTest(t)
	bin := buildBinary(t)
	upstream := endlessStream(t)

	addr := deadListener(t)
	cmd, stderr := startBinary(t, bin, addr, upstream.URL, t.TempDir())
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer cmd.Process.Kill() // no-op once it has exited

	// Hold a stream open through the proxy so the drain cannot finish.
	resp, err := http.Get("http://" + addr + "/v1/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatalf("no event relayed before the shutdown test began: %v", err)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		t.Fatalf("exited on the first signal (%v) instead of waiting for the open stream\nstderr: %s", err, stderr.String())
	case <-time.After(time.Second):
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited: // any exit passes: the signal reached its default disposition
	case <-time.After(10 * time.Second):
		t.Fatalf("second SIGTERM was swallowed; the process is still draining\nstderr: %s", stderr.String())
	}
}

// TestE2EUnwritableLogDirWarns covers the startup check: the proxy still
// serves, and the operator is told immediately instead of finding an empty
// audit trail weeks later.
func TestE2EUnwritableLogDirWarns(t *testing.T) {
	requireBinaryTest(t)
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits are advisory")
	}
	bin := buildBinary(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o700) })

	addr := deadListener(t)
	cmd, stderr := startBinary(t, bin, addr, upstream.URL, filepath.Join(parent, "logs"))
	defer func() {
		cmd.Process.Signal(syscall.SIGTERM)
		cmd.Wait()
	}()

	if out := stderr.String(); !strings.Contains(out, "WARNING") || !strings.Contains(out, "not writable") {
		t.Errorf("no startup warning about the unwritable log directory; stderr:\n%s", out)
	}
}
