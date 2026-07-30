package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/demirtechcom/iold/internal/state"
	"github.com/demirtechcom/iold/internal/supervisor"
)

// seedRecoveryDeployment creates one deployment record in the state
// store under dir and returns after closing the store, simulating a
// prior CLI invocation that has since exited.
func seedRecoveryDeployment(t *testing.T, dir, id string) {
	t.Helper()
	store := openRecoveryStore(t, dir)
	defer store.Close()
	_, err := store.Create(state.Deployment{
		ID:               id,
		Alias:            "qwen3.6-35b-a3b",
		Artifact:         "unsloth/Qwen3.6-35B-A3B-NVFP4-Fast",
		ArtifactRevision: "rev",
		Port:             8000,
		IdempotencyKey:   "idem-" + id,
	})
	if err != nil {
		t.Fatalf("seed deployment %s: %v", id, err)
	}
}

func openRecoveryStore(t *testing.T, dir string) *state.Store {
	t.Helper()
	store, err := state.Open(filepath.Join(dir, "iold.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store
}

// walkPhases applies each transition in order, failing the test on any
// illegal edge. It simulates the lifecycle progress a previous CLI run
// persisted before it was interrupted.
func walkPhases(t *testing.T, store *state.Store, id string, phases ...state.Phase) {
	t.Helper()
	walkPhasesFrom(t, store, id, state.PhaseRequested, phases...)
}

func walkPhasesFrom(t *testing.T, store *state.Store, id string, from state.Phase, phases ...state.Phase) {
	t.Helper()
	for _, to := range phases {
		if err := store.Transition(id, from, to, ""); err != nil {
			t.Fatalf("transition %s: %s -> %s: %v", id, from, to, err)
		}
		from = to
	}
}

func toReady(t *testing.T, store *state.Store, id string) {
	t.Helper()
	walkPhases(t, store, id,
		state.PhaseValidating, state.PhaseDownloading, state.PhaseStarting,
		state.PhaseHealthy, state.PhaseRegistering, state.PhaseReady)
}

// TestDestroyResumesAfterInterrupt covers docs/ARCHITECTURE.md §6/§9: a CLI
// killed after persisting DESTROYING must be resumable — the next
// `iold destroy` invocation completes the remaining steps.
func TestDestroyResumesAfterInterrupt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IOLD_STATE_DIR", dir)
	seedRecoveryDeployment(t, dir, "dep-int")

	logFile := filepath.Join(dir, "logs", "dep-int.log")
	if err := os.MkdirAll(filepath.Dir(logFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logFile, []byte("runtime log\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	store := openRecoveryStore(t, dir)
	toReady(t, store, "dep-int")
	if err := store.Transition("dep-int", state.PhaseReady, state.PhaseDestroying, ""); err != nil {
		t.Fatalf("READY -> DESTROYING: %v", err)
	}
	store.Close() // the interrupted CLI is gone

	var stdout, stderr bytes.Buffer
	if err := Run([]string{"destroy", "dep-int"}, &stdout, &stderr); err != nil {
		t.Fatalf("destroy after interrupt returned error: %v\n%s", err, stdout.String())
	}
	if _, err := os.Stat(logFile); !os.IsNotExist(err) {
		t.Fatalf("owned log file not removed: %v", err)
	}

	store = openRecoveryStore(t, dir)
	defer store.Close()
	got, err := store.Get("dep-int")
	if err != nil {
		t.Fatalf("Get after destroy: %v", err)
	}
	if got.Phase != state.PhaseDestroyed {
		t.Fatalf("phase = %s, want %s", got.Phase, state.PhaseDestroyed)
	}
}

// TestDestroyResumeStopsLiveRuntime covers a crash between persisting
// DESTROYING and terminating the runtime: the recorded PID is still
// alive and the resumed destroy must stop it before finishing.
func TestDestroyResumeStopsLiveRuntime(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IOLD_STATE_DIR", dir)
	seedRecoveryDeployment(t, dir, "dep-live")

	// The loop keeps sh itself alive; a single command would be
	// exec-replaced and change the process's command line.
	proc, err := supervisor.Start(supervisor.Spec{
		Command: "sh",
		Args:    []string{"-c", "while true; do sleep 0.1; done"},
		LogPath: filepath.Join(dir, "logs", "dep-live.log"),
	})
	if err != nil {
		t.Fatalf("start runtime: %v", err)
	}

	store := openRecoveryStore(t, dir)
	attachTestRuntime(t, store, "dep-live", proc, 8000)
	walkPhasesFrom(t, store, "dep-live", state.PhaseStarting,
		state.PhaseHealthy, state.PhaseRegistering, state.PhaseReady)
	if err := store.Transition("dep-live", state.PhaseReady, state.PhaseDestroying, ""); err != nil {
		t.Fatalf("READY -> DESTROYING: %v", err)
	}
	store.Close() // crash before the process was stopped

	var stdout, stderr bytes.Buffer
	if err := Run([]string{"destroy", "dep-live"}, &stdout, &stderr); err != nil {
		t.Fatalf("destroy returned error: %v\n%s", err, stdout.String())
	}
	if supervisor.Alive(proc.PID) {
		t.Fatalf("runtime process %d still alive after resumed destroy", proc.PID)
	}

	store = openRecoveryStore(t, dir)
	defer store.Close()
	got, err := store.Get("dep-live")
	if err != nil {
		t.Fatalf("Get after destroy: %v", err)
	}
	if got.Phase != state.PhaseDestroyed {
		t.Fatalf("phase = %s, want %s", got.Phase, state.PhaseDestroyed)
	}
}

// TestStatusReconcilesDeadRuntimeAsCrashed covers crash/reopen recovery: a
// deployment with a dead recorded runtime must not remain STARTING.
func TestStatusReconcilesDeadRuntimeAsCrashed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IOLD_STATE_DIR", dir)
	seedRecoveryDeployment(t, dir, "dep-crash")

	proc, err := supervisor.Start(supervisor.Spec{
		Command: "sh",
		Args:    []string{"-c", "exit 0"},
		LogPath: filepath.Join(dir, "logs", "dep-crash.log"),
	})
	if err != nil {
		t.Fatalf("start short-lived process: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for supervisor.Alive(proc.PID) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d did not exit in time", proc.PID)
		}
		time.Sleep(10 * time.Millisecond)
	}

	store := openRecoveryStore(t, dir)
	attachTestRuntime(t, store, "dep-crash", proc, 8000)
	store.Close() // simulated crash; status reopens the database

	var stdout, stderr bytes.Buffer
	if err := Run([]string{"status", "--json"}, &stdout, &stderr); err != nil {
		t.Fatalf("status returned error: %v\n%s", err, stdout.String())
	}
	var deployments []state.Deployment
	if err := json.Unmarshal(stdout.Bytes(), &deployments); err != nil {
		t.Fatalf("status output is not valid JSON: %v\n%s", err, stdout.String())
	}
	if len(deployments) != 1 {
		t.Fatalf("got %d deployments, want 1: %+v", len(deployments), deployments)
	}
	got := deployments[0]
	if got.ID != "dep-crash" || got.Phase != state.PhaseCrashed {
		t.Fatalf("deployment was not reconciled as CRASHED: %+v", got)
	}
	if got.PID != proc.PID || got.Port != 8000 {
		t.Fatalf("runtime fields lost after crash: pid=%d port=%d, want pid=%d port=8000",
			got.PID, got.Port, proc.PID)
	}
}

func TestStatusAdoptsProcessStartedAfterPersistedIntent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IOLD_STATE_DIR", dir)
	seedRecoveryDeployment(t, dir, "dep-intent")
	args := []string{"-c", "while true; do sleep 0.1; done"}
	intent := runtimeCommandString("sh", args)
	store := openRecoveryStore(t, dir)
	walkPhases(t, store, "dep-intent", state.PhaseValidating, state.PhaseDownloading)
	if err := store.SetRuntimeIntent("dep-intent", 8000, intent); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition("dep-intent", state.PhaseDownloading, state.PhaseStarting, ""); err != nil {
		t.Fatal(err)
	}
	store.Close()

	proc, err := supervisor.Start(supervisor.Spec{
		Command: "sh", Args: args,
		LogPath: filepath.Join(dir, "logs", "dep-intent.log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Stop(proc, 200*time.Millisecond) })

	var stdout, stderr bytes.Buffer
	if err := Run([]string{"status", "dep-intent", "--json"}, &stdout, &stderr); err != nil {
		t.Fatalf("status: %v", err)
	}
	store = openRecoveryStore(t, dir)
	adopted, err := store.Get("dep-intent")
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if adopted.Phase != state.PhaseCrashed || adopted.PID != proc.PID ||
		adopted.StartToken == "" || adopted.StartedAt.IsZero() {
		t.Fatalf("STARTING intent was not safely adopted: %+v", adopted)
	}

	stdout.Reset()
	if err := Run([]string{"destroy", "dep-intent"}, &stdout, &stderr); err != nil {
		t.Fatalf("destroy adopted runtime: %v", err)
	}
	if supervisor.Alive(proc.PID) {
		t.Fatalf("adopted runtime %d survived destroy", proc.PID)
	}
}

// TestDestroyAllMixedPhases covers `destroy --all` over one deployment
// that is already DESTROYED and one still in REQUESTED: both must end
// DESTROYED and the billing warning must print exactly once.
func TestDestroyAllMixedPhases(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IOLD_STATE_DIR", dir)
	seedRecoveryDeployment(t, dir, "dep-done")
	seedRecoveryDeployment(t, dir, "dep-new")

	store := openRecoveryStore(t, dir)
	walkPhases(t, store, "dep-done",
		state.PhaseFailed, state.PhaseDestroying, state.PhaseDestroyed)
	store.Close()

	var stdout, stderr bytes.Buffer
	if err := Run([]string{"destroy", "--all"}, &stdout, &stderr); err != nil {
		t.Fatalf("destroy --all returned error: %v\n%s", err, stdout.String())
	}
	if got := strings.Count(stdout.String(), "billing continues"); got != 1 {
		t.Fatalf("billing warning printed %d times, want 1:\n%s", got, stdout.String())
	}

	store = openRecoveryStore(t, dir)
	defer store.Close()
	for _, id := range []string{"dep-done", "dep-new"} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Phase != state.PhaseDestroyed {
			t.Fatalf("%s phase = %s, want %s", id, got.Phase, state.PhaseDestroyed)
		}
	}
}
