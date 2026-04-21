//go:build interop

package interop

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

// Run interop tests with: `make interop-up && go test -tags interop ./tests/interop/...`.

const (
	hostEndpoint = "tcp/127.0.0.1:7447"
	readyTimeout = 10 * time.Second
	ioTimeout    = 15 * time.Second
	// Line-protocol sentinels — keep in sync with python_common.py.
	readyMarker = "READY"
	doneMarker  = "DONE"
	goMarker    = "GO"
)

// composeFile is resolved relative to this test file's directory.
var composeFile = func() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return filepath.Join(wd, "..", "docker-compose.yml")
}()

// requireZenohd skips the test unless tcp/127.0.0.1:7447 accepts a TCP dial.
// If the Makefile target hasn't been run, we'd rather skip than fail loudly.
func requireZenohd(t *testing.T) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:7447", 2*time.Second)
	if err != nil {
		t.Skipf("zenohd not reachable at 127.0.0.1:7447 (run `make interop-up` first): %v", err)
	}
	_ = conn.Close()
}

// pyProc wraps a docker-compose-exec'd Python script with stdin/stdout plumbing.
type pyProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	lines  chan string // decoded stdout lines (one per line)
	errs   chan error  // stdout-reader error or EOF
	done   chan struct{}
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// startPython launches `docker compose exec -T python python /work/<script>` with args.
func startPython(t *testing.T, script string, args ...string) *pyProc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	full := append([]string{
		"compose", "-f", composeFile, "exec", "-T", "python",
		"python", "/work/" + script,
	}, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("docker compose exec python: %v", err)
	}

	p := &pyProc{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
		lines:  make(chan string, 64),
		errs:   make(chan error, 1),
		done:   make(chan struct{}),
		cancel: cancel,
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer close(p.lines)
		for {
			line, err := p.stdout.ReadString('\n')
			if line != "" {
				// Guard the send against close(t) so a stuck test never
				// deadlocks this goroutine on a full channel.
				select {
				case p.lines <- strings.TrimRight(line, "\n"):
				case <-p.done:
					return
				}
			}
			if err != nil {
				p.errs <- err
				return
			}
		}
	}()
	return p
}

// waitFor reads lines until it sees want or the timeout fires.
// Unexpected lines in between are logged but tolerated.
func (p *pyProc) waitFor(t *testing.T, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case line, ok := <-p.lines:
			if !ok {
				t.Fatalf("python stdout closed while waiting for %q", want)
			}
			if line == want {
				return
			}
			t.Logf("python > %s (waiting for %q)", line, want)
		case err := <-p.errs:
			if err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("python stdout error: %v", err)
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %q from python", want)
		}
	}
}

// readLine returns the next line or fails on timeout.
func (p *pyProc) readLine(t *testing.T, timeout time.Duration) string {
	t.Helper()
	select {
	case line, ok := <-p.lines:
		if !ok {
			t.Fatalf("python stdout closed unexpectedly")
		}
		return line
	case <-time.After(timeout):
		t.Fatalf("timeout waiting for a line from python")
	}
	return ""
}

// signalGo writes "GO\n" to stdin — matches python_pub.py's gate.
func (p *pyProc) signalGo(t *testing.T) {
	t.Helper()
	if _, err := io.WriteString(p.stdin, goMarker+"\n"); err != nil {
		t.Fatalf("write %s: %v", goMarker, err)
	}
}

// close closes stdin (triggering the script's EOF-exit path) and waits for
// the process. Errors are logged, not failed, so a lingering python exit
// code doesn't mask the real test failure.
func (p *pyProc) close(t *testing.T) {
	t.Helper()
	close(p.done) // wake a send-blocked reader, if any
	_ = p.stdin.Close()
	waitErr := make(chan error, 1)
	go func() { waitErr <- p.cmd.Wait() }()
	select {
	case err := <-waitErr:
		if err != nil && !errors.As(err, new(*exec.ExitError)) {
			t.Logf("python wait: %v", err)
		}
	case <-time.After(5 * time.Second):
		p.cancel()
		t.Logf("python did not exit in time; killed")
		<-waitErr
	}
	p.wg.Wait()
}

// openGoSession connects to the host-exposed zenohd at 127.0.0.1:7447.
func openGoSession(t *testing.T) *zenoh.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := zenoh.NewConfig().WithEndpoint(hostEndpoint)
	s, err := zenoh.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("zenoh.Open: %v", err)
	}
	return s
}

// pyRecord matches the JSON schema emitted by python_common.sample_to_json /
// reply_to_json. Fields not produced by a given helper are left as the zero value.
type pyRecord struct {
	Kind    string `json:"kind"`
	Key     string `json:"key"`
	Payload string `json:"payload"` // base64
}

// decodeRecord parses one JSON line into pyRecord and b64-decodes its payload.
func decodeRecord(t *testing.T, line string) (pyRecord, []byte) {
	t.Helper()
	var r pyRecord
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	raw, err := base64.StdEncoding.DecodeString(r.Payload)
	if err != nil {
		t.Fatalf("b64 %q: %v", r.Payload, err)
	}
	return r, raw
}

// TestGoPubPySub: Go publishes N messages, Python subscriber receives them.
func TestGoPubPySub(t *testing.T) {
	requireZenohd(t)

	const (
		key   = "interop/go2py"
		count = 5
	)

	sub := startPython(t, "python_sub.py", "--key", key, "--count", fmt.Sprint(count))
	defer sub.close(t)
	sub.waitFor(t, readyMarker, readyTimeout)

	session := openGoSession(t)
	defer session.Close()

	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}
	pub, err := session.DeclarePublisher(ke, nil)
	if err != nil {
		t.Fatalf("DeclarePublisher: %v", err)
	}
	defer pub.Drop()

	// Give the router a breath to propagate the D_SUBSCRIBER before PUSH.
	time.Sleep(200 * time.Millisecond)

	for i := 0; i < count; i++ {
		if err := pub.Put(zenoh.NewZBytesFromString(fmt.Sprintf("hello-%d", i)), nil); err != nil {
			t.Fatalf("pub.Put[%d]: %v", i, err)
		}
	}

	for i := 0; i < count; i++ {
		line := sub.readLine(t, ioTimeout)
		if line == doneMarker {
			t.Fatalf("%s before %d samples; got %d", doneMarker, count, i)
		}
		_, payload := decodeRecord(t, line)
		want := fmt.Sprintf("hello-%d", i)
		if string(payload) != want {
			t.Errorf("sample[%d] = %q, want %q", i, payload, want)
		}
	}
	sub.waitFor(t, doneMarker, ioTimeout)
}

// TestPyPubGoSub: Python publishes N messages, Go subscriber receives them.
func TestPyPubGoSub(t *testing.T) {
	requireZenohd(t)

	const (
		key   = "interop/py2go"
		count = 5
	)

	session := openGoSession(t)
	defer session.Close()

	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}
	ch := make(chan zenoh.Sample, count+1)
	sub, err := session.DeclareSubscriber(ke, zenoh.Closure[zenoh.Sample]{
		Call: func(s zenoh.Sample) { ch <- s },
	})
	if err != nil {
		t.Fatalf("DeclareSubscriber: %v", err)
	}
	defer sub.Drop()

	pub := startPython(t, "python_pub.py", "--key", key, "--count", fmt.Sprint(count))
	defer pub.close(t)
	pub.waitFor(t, readyMarker, readyTimeout)
	// Router needs a moment to propagate the D_SUBSCRIBER.
	time.Sleep(200 * time.Millisecond)
	pub.signalGo(t)

	deadline := time.After(ioTimeout)
	for i := 0; i < count; i++ {
		select {
		case s := <-ch:
			got := string(s.Payload().Bytes())
			want := fmt.Sprintf("msg-%d", i)
			if got != want {
				t.Errorf("sample[%d] = %q, want %q", i, got, want)
			}
		case <-deadline:
			t.Fatalf("timeout at sample %d/%d", i, count)
		}
	}
	pub.waitFor(t, doneMarker, ioTimeout)
}

// TestGoGetPyQueryable: Go sends Get; Python queryable replies.
func TestGoGetPyQueryable(t *testing.T) {
	requireZenohd(t)

	const key = "interop/g2pqbl"

	qbl := startPython(t, "python_queryable.py",
		"--key", key, "--reply-payload", "pong-from-python")
	defer qbl.close(t)
	qbl.waitFor(t, readyMarker, readyTimeout)

	session := openGoSession(t)
	defer session.Close()

	// Let the router propagate D_QUERYABLE.
	time.Sleep(200 * time.Millisecond)

	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}
	replies, err := session.Get(ke, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	deadline := time.After(ioTimeout)
	var payloads []string
	for open := true; open; {
		select {
		case r, ok := <-replies:
			if !ok {
				open = false
				continue
			}
			s, hasSample := r.Sample()
			if !hasSample {
				t.Errorf("expected ok reply, got err")
				continue
			}
			payloads = append(payloads, string(s.Payload().Bytes()))
		case <-deadline:
			t.Fatal("timeout waiting for reply or final")
		}
	}
	if len(payloads) != 1 || payloads[0] != "pong-from-python" {
		t.Errorf("replies = %v, want [\"pong-from-python\"]", payloads)
	}
}

// TestGoLivelinessPyGet: Go declares a liveliness token, Python liveliness.get sees it.
func TestGoLivelinessPyGet(t *testing.T) {
	requireZenohd(t)

	const (
		tokenKey = "interop/live/alpha"
		getKey   = "interop/live/**"
	)

	session := openGoSession(t)
	defer session.Close()

	ke, err := zenoh.NewKeyExpr(tokenKey)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}
	tok, err := session.Liveliness().DeclareToken(ke, nil)
	if err != nil {
		t.Fatalf("DeclareToken: %v", err)
	}
	defer tok.Drop()

	// Router needs a moment to propagate D_TOKEN before the Python get lands.
	time.Sleep(200 * time.Millisecond)

	get := startPython(t, "python_liveliness_get.py", "--key", getKey)
	defer get.close(t)
	get.waitFor(t, readyMarker, readyTimeout)

	var keys []string
	for {
		line := get.readLine(t, ioTimeout)
		if line == doneMarker {
			break
		}
		rec, _ := decodeRecord(t, line)
		keys = append(keys, rec.Key)
	}
	found := false
	for _, k := range keys {
		if k == tokenKey {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("liveliness get returned %v, want one entry %q", keys, tokenKey)
	}
}

// TestPyGetGoQueryable: Python sends Get; Go queryable replies.
func TestPyGetGoQueryable(t *testing.T) {
	requireZenohd(t)

	const (
		key   = "interop/pg2qbl"
		reply = "pong-from-go"
	)

	session := openGoSession(t)
	defer session.Close()

	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}
	qbl, err := session.DeclareQueryable(ke, func(q *zenoh.Query) {
		if err := q.Reply(ke, zenoh.NewZBytesFromString(reply), nil); err != nil {
			t.Errorf("q.Reply: %v", err)
		}
	}, nil)
	if err != nil {
		t.Fatalf("DeclareQueryable: %v", err)
	}
	defer qbl.Drop()

	// Let the router propagate D_QUERYABLE.
	time.Sleep(200 * time.Millisecond)

	get := startPython(t, "python_get.py", "--key", key)
	defer get.close(t)
	get.waitFor(t, readyMarker, readyTimeout)

	// Expect exactly one JSON line then DONE.
	line := get.readLine(t, ioTimeout)
	if line == doneMarker {
		t.Fatal("DONE with no reply from Go queryable")
	}
	rec, payload := decodeRecord(t, line)
	if rec.Kind != "ok" {
		t.Errorf("reply kind = %q, want ok", rec.Kind)
	}
	if string(payload) != reply {
		t.Errorf("payload = %q, want %q", payload, reply)
	}
	get.waitFor(t, doneMarker, ioTimeout)
}
