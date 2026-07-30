package supervisor

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func startShell(t *testing.T, script string) Process {
	t.Helper()
	proc, err := Start(Spec{
		Command: "sh",
		Args:    []string{"-c", script},
		LogPath: filepath.Join(t.TempDir(), "logs", "run.log"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = Stop(proc, 200*time.Millisecond) })
	return proc
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg)
}

func TestStartCapturesLogsWithRestrictivePermissions(t *testing.T) {
	proc := startShell(t, `echo "hello from runtime"; sleep 30`)
	waitFor(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(proc.LogPath)
		return err == nil && strings.Contains(string(data), "hello from runtime")
	}, "log output never appeared")

	info, err := os.Stat(proc.LogPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("log file mode = %o, want 600", perm)
	}
	if Reconcile(proc) != StatusRunning {
		t.Fatalf("running process reconciled as %s", Reconcile(proc))
	}
}

func TestStopGracefulOnSIGTERM(t *testing.T) {
	proc := startShell(t, `trap 'exit 0' TERM; while true; do sleep 0.05; done`)
	waitFor(t, 5*time.Second, func() bool { return Alive(proc.PID) }, "process never came up")

	start := time.Now()
	if err := Stop(proc, 5*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if Alive(proc.PID) {
		t.Fatal("process still alive after Stop")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("graceful stop took %v; SIGTERM was likely ignored", elapsed)
	}
}

func TestStopForcesKillWhenTERMIgnored(t *testing.T) {
	proc := startShell(t, `trap '' TERM; while true; do sleep 0.05; done`)
	waitFor(t, 5*time.Second, func() bool { return Alive(proc.PID) }, "process never came up")

	if err := Stop(proc, 300*time.Millisecond); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if Alive(proc.PID) {
		t.Fatal("process survived SIGKILL escalation")
	}
}

func TestReconcileStalePID(t *testing.T) {
	proc := startShell(t, `exit 0`)
	waitFor(t, 5*time.Second, func() bool { return !Alive(proc.PID) }, "process never exited")
	if status := Reconcile(proc); status != StatusStale {
		t.Fatalf("Reconcile = %s, want STALE", status)
	}
}

func TestReconcileDetectsPIDReuse(t *testing.T) {
	// The test binary's own PID is guaranteed alive but runs a different
	// command line than the recorded one.
	startedAt, token, err := processStartInfo(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	impostor := Process{
		PID: os.Getpid(), Command: "vllm serve some-model --port 8000",
		StartedAt: startedAt, StartToken: token,
	}
	if status := Reconcile(impostor); status != StatusReused {
		t.Fatalf("Reconcile = %s, want REUSED", status)
	}
}

func TestStopRefusesUnownedPID(t *testing.T) {
	startedAt, token, identityErr := processStartInfo(os.Getpid())
	if identityErr != nil {
		t.Fatal(identityErr)
	}
	impostor := Process{
		PID: os.Getpid(), Command: "vllm serve some-model --port 8000",
		StartedAt: startedAt, StartToken: token,
	}
	err := Stop(impostor, time.Second)
	if !errors.Is(err, ErrNotOwned) {
		t.Fatalf("expected ErrNotOwned, got %v", err)
	}
	if !Alive(os.Getpid()) {
		t.Fatal("test process was signalled")
	}
}

func TestReconcileRejectsSameCommandWithDifferentStartIdentity(t *testing.T) {
	current, err := cmdline(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	startedAt, _, err := processStartInfo(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	impostor := Process{
		PID: os.Getpid(), Command: normalize(current), StartedAt: startedAt,
		StartToken: "definitely-not-this-process",
	}
	if status := Reconcile(impostor); status != StatusReused {
		t.Fatalf("Reconcile = %s, want REUSED", status)
	}
}

func TestCommandIntentMatchingRequiresExactArgv(t *testing.T) {
	tests := []struct {
		name    string
		current string
		intent  string
		want    bool
	}{
		{"exact", "vllm serve org/model --port 8000", "vllm serve org/model --port 8000", true},
		{"resolved executable", "/opt/bin/vllm serve org/model --port 8000", "vllm serve org/model --port 8000", true},
		{"python entrypoint", "python3 /opt/bin/vllm serve org/model --port 8000", "vllm serve org/model --port 8000", true},
		{"different executable", "other serve org/model --port 8000", "vllm serve org/model --port 8000", false},
		{"extra prefix", "unrelated --flag vllm serve org/model --port 8000", "vllm serve org/model --port 8000", false},
		{"different argument", "vllm serve org/other --port 8000", "vllm serve org/model --port 8000", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := matchesCommandIntent(test.current, test.intent); got != test.want {
				t.Fatalf("matchesCommandIntent(%q, %q) = %v, want %v", test.current, test.intent, got, test.want)
			}
		})
	}
}

func TestStopWaitsForAndKillsWorkerProcessGroup(t *testing.T) {
	proc := startShell(t, `trap 'exit 0' TERM; sh -c 'trap "" TERM; while true; do sleep 1; done' & while true; do sleep 0.05; done`)
	waitFor(t, 5*time.Second, func() bool { return processGroupAlive(proc.PID) }, "process group never came up")
	if err := Stop(proc, 200*time.Millisecond); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if processGroupAlive(proc.PID) {
		t.Fatal("worker survived process-group SIGKILL escalation")
	}
}

func TestStartRedactsLogsBeforeWritingToDisk(t *testing.T) {
	proc := startShell(t, `echo '{"Authorization":"Bearer raw-secret-value"}'; sleep 30`)
	waitFor(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(proc.LogPath)
		return err == nil && strings.Contains(string(data), "[REDACTED]")
	}, "redacted log output never appeared")
	data, err := os.ReadFile(proc.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "raw-secret-value") {
		t.Fatalf("raw credential reached disk: %s", data)
	}
}

func TestStopOnStalePIDSucceeds(t *testing.T) {
	proc := startShell(t, `exit 0`)
	waitFor(t, 5*time.Second, func() bool { return !Alive(proc.PID) }, "process never exited")
	if err := Stop(proc, time.Second); err != nil {
		t.Fatalf("Stop on stale PID: %v", err)
	}
}

func TestStartFailsForMissingBinary(t *testing.T) {
	_, err := Start(Spec{
		Command: "/nonexistent/iold-runtime",
		LogPath: filepath.Join(t.TempDir(), "run.log"),
	})
	if !errors.Is(err, ErrStartFail) {
		t.Fatalf("expected ErrStartFail, got %v", err)
	}
}

func TestAllocatePortSkipsBoundAndReservedPorts(t *testing.T) {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	bound := listener.Addr().(*net.TCPAddr).Port

	if PortFree(bound) {
		t.Fatalf("PortFree(%d) = true for a bound port", bound)
	}

	got, err := AllocatePort(bound, []int{bound + 1})
	if err != nil {
		t.Fatalf("AllocatePort: %v", err)
	}
	if got == bound || got == bound+1 {
		t.Fatalf("AllocatePort returned unavailable port %d", got)
	}
	if got < bound || got >= bound+portScanWindow {
		t.Fatalf("AllocatePort returned %d outside scan window starting at %d", got, bound)
	}
}

func TestAllocatePortExhaustion(t *testing.T) {
	reserved := make([]int, portScanWindow)
	for i := range reserved {
		reserved[i] = 40000 + i
	}
	if _, err := AllocatePort(40000, reserved); !errors.Is(err, ErrNoFreePort) {
		t.Fatalf("expected ErrNoFreePort, got %v", err)
	}
}

func TestProcessSurvivesLogFileHandleClosure(t *testing.T) {
	// Start closes its handle to the log file immediately; the child must
	// keep writing through its inherited descriptor.
	proc := startShell(t, `for i in 1 2 3; do echo "line $i"; sleep 0.05; done; sleep 30`)
	waitFor(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(proc.LogPath)
		return err == nil && strings.Contains(string(data), "line 3")
	}, fmt.Sprintf("child stopped writing to %s", proc.LogPath))
}
