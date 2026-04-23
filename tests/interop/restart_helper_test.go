//go:build interop

package interop

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// restartZenohd restarts the zenohd container via docker compose and
// blocks until zenohd's healthcheck reports ready again.
//
// The short -t 1 SIGTERM grace keeps the router off the wire briefly — long
// enough for the Go session's reconnect orchestrator to observe the
// disconnect and enter its backoff loop — but not so long the test waits
// tens of seconds for reconnect to succeed.
func restartZenohd(t *testing.T) {
	t.Helper()
	// Healthcheck grace is retries*interval = 15*2 = 30s; give docker
	// compose double that to cover slow CI and cold-start image pulls.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker",
		"compose", "-f", composeFile, "restart", "-t", "1", "zenohd")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker compose restart zenohd: %v\n%s", err, out)
	}
}
