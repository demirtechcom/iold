package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/demirtechcom/iold/internal/catalog"
	"github.com/demirtechcom/iold/internal/doctor"
	"github.com/demirtechcom/iold/internal/state"
	"github.com/demirtechcom/iold/internal/supervisor"
)

func TestModelsListsFirstArtifact(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := Run([]string{"models"}, &stdout, &stderr); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !strings.Contains(stdout.String(), "unsloth/Qwen3.6-35B-A3B-NVFP4-Fast") {
		t.Fatalf("models output did not contain artifact: %q", stdout.String())
	}
}

func TestAddIsNotImplementedYet(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run([]string{"add", "qwen3.6-35b-a3b"}, &stdout, &stderr)
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("expected ErrNotImplemented, got %v", err)
	}
}

func TestDeployRejectsUnknownNonHFReference(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run([]string{"deploy", "not-a-model"}, &stdout, &stderr)
	if !errors.Is(err, catalog.ErrUnknownModel) {
		t.Fatalf("expected ErrUnknownModel, got %v", err)
	}
}

type fakeProbes struct {
	gpus     []doctor.GPU
	diskFree uint64
}

func (f fakeProbes) GPUs() ([]doctor.GPU, error)          { return f.gpus, nil }
func (f fakeProbes) DiskFreeBytes(string) (uint64, error) { return f.diskFree, nil }
func (f fakeProbes) CheckEndpoint(string) error           { return nil }

func TestDoctorReportsCatalogVRAMRequirement(t *testing.T) {
	var stdout bytes.Buffer
	probes := fakeProbes{
		gpus: []doctor.GPU{{
			Name:            "NVIDIA RTX PRO 6000 Blackwell Workstation Edition",
			DriverVersion:   "575.51.03",
			VRAMMiB:         97887,
			ComputeCapMajor: 12,
		}},
		diskFree: 500 << 30,
	}
	if err := runDoctor(probes, &stdout); err != nil {
		t.Fatalf("runDoctor returned error: %v\n%s", err, stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "32768 MiB required") {
		t.Fatalf("output missing catalog VRAM requirement:\n%s", out)
	}
	if !strings.Contains(out, "All checks passed.") {
		t.Fatalf("output missing success line:\n%s", out)
	}
}

func TestDoctorFailsOnInsufficientVRAM(t *testing.T) {
	var stdout bytes.Buffer
	probes := fakeProbes{
		gpus: []doctor.GPU{{
			Name:            "NVIDIA RTX PRO 6000 Blackwell Workstation Edition",
			DriverVersion:   "575.51.03",
			VRAMMiB:         16384,
			ComputeCapMajor: 12,
		}},
		diskFree: 500 << 30,
	}
	err := runDoctor(probes, &stdout)
	if !errors.Is(err, doctor.ErrChecksFailed) {
		t.Fatalf("expected ErrChecksFailed, got %v", err)
	}
}

func TestStatusEmptyStore(t *testing.T) {
	t.Setenv("IOLD_STATE_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer
	if err := Run([]string{"status"}, &stdout, &stderr); err != nil {
		t.Fatalf("status returned error: %v", err)
	}
	if !strings.Contains(stdout.String(), "No deployments") {
		t.Fatalf("unexpected output: %q", stdout.String())
	}
}

func seedDeployment(t *testing.T, dir string) {
	t.Helper()
	cacheDir, err := deploymentModelCacheDir("dep-1")
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(dir, "iold.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	_, err = store.Create(state.Deployment{
		ID:               "dep-1",
		Alias:            "qwen3.6-35b-a3b",
		Artifact:         "unsloth/Qwen3.6-35B-A3B-NVFP4-Fast",
		ArtifactRevision: "rev",
		ModelCacheDir:    cacheDir,
		Port:             8000,
		IdempotencyKey:   "idem-1",
	})
	if err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
}

func attachTestRuntime(t *testing.T, store *state.Store, id string, proc supervisor.Process, port int) {
	t.Helper()
	for _, step := range []struct{ from, to state.Phase }{
		{state.PhaseRequested, state.PhaseValidating},
		{state.PhaseValidating, state.PhaseDownloading},
	} {
		if err := store.Transition(id, step.from, step.to, ""); err != nil {
			t.Fatalf("Transition %s -> %s: %v", step.from, step.to, err)
		}
	}
	if err := store.SetRuntimeIntent(id, port, proc.Command); err != nil {
		t.Fatalf("SetRuntimeIntent: %v", err)
	}
	if err := store.Transition(id, state.PhaseDownloading, state.PhaseStarting, ""); err != nil {
		t.Fatalf("Transition to STARTING: %v", err)
	}
	if err := store.SetRuntime(id, proc.PID, port, proc.Command, proc.StartedAt, proc.StartToken); err != nil {
		t.Fatalf("SetRuntime: %v", err)
	}
}

func TestStatusListsDeployment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IOLD_STATE_DIR", dir)
	seedDeployment(t, dir)

	var stdout, stderr bytes.Buffer
	if err := Run([]string{"status"}, &stdout, &stderr); err != nil {
		t.Fatalf("status returned error: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "dep-1") || !strings.Contains(out, "CRASHED") {
		t.Fatalf("output missing deployment row:\n%s", out)
	}
}

func TestStatusJSONOutput(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IOLD_STATE_DIR", dir)
	seedDeployment(t, dir)

	var stdout, stderr bytes.Buffer
	if err := Run([]string{"status", "dep-1", "--json"}, &stdout, &stderr); err != nil {
		t.Fatalf("status returned error: %v", err)
	}
	var deployments []state.Deployment
	if err := json.Unmarshal(stdout.Bytes(), &deployments); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, stdout.String())
	}
	if len(deployments) != 1 || deployments[0].ID != "dep-1" {
		t.Fatalf("unexpected JSON payload: %+v", deployments)
	}
}

func TestStatusUnknownDeploymentFails(t *testing.T) {
	t.Setenv("IOLD_STATE_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer
	err := Run([]string{"status", "missing"}, &stdout, &stderr)
	if !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestLogsRedactsSecrets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IOLD_STATE_DIR", dir)
	seedDeployment(t, dir)
	logFile := filepath.Join(dir, "logs", "dep-1.log")
	if err := os.MkdirAll(filepath.Dir(logFile), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "engine ready on port 8000\nAuthorization: Bearer verysecrettoken\n"
	if err := os.WriteFile(logFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := Run([]string{"logs"}, &stdout, &stderr); err != nil {
		t.Fatalf("logs returned error: %v", err)
	}
	out := stdout.String()
	if strings.Contains(out, "verysecrettoken") {
		t.Fatalf("secret leaked in logs output:\n%s", out)
	}
	if !strings.Contains(out, "engine ready on port 8000") {
		t.Fatalf("log content missing:\n%s", out)
	}
}

func TestLogsMissingDeploymentFails(t *testing.T) {
	t.Setenv("IOLD_STATE_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer
	if err := Run([]string{"logs"}, &stdout, &stderr); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDestroyStopsProcessRemovesDataAndWarnsAboutBilling(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IOLD_STATE_DIR", dir)
	cacheRoot := filepath.Join(dir, "model-cache")
	t.Setenv("IOLD_MODEL_CACHE", cacheRoot)
	seedDeployment(t, dir)
	cachePath := filepath.Join(cacheRoot, "dep-1")
	if err := prepareModelCache(cachePath, "dep-1", "idem-1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cachePath, "weights.bin"), []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The loop keeps sh itself alive; a single command would be
	// exec-replaced and change the process's command line.
	proc, err := supervisor.Start(supervisor.Spec{
		Command: "sh",
		Args:    []string{"-c", "while true; do sleep 0.1; done"},
		LogPath: filepath.Join(dir, "logs", "dep-1.log"),
	})
	if err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	store, err := state.Open(filepath.Join(dir, "iold.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	attachTestRuntime(t, store, "dep-1", proc, 8000)
	store.Close()

	var stdout, stderr bytes.Buffer
	if err := Run([]string{"destroy", "dep-1"}, &stdout, &stderr); err != nil {
		t.Fatalf("destroy returned error: %v\n%s", err, stdout.String())
	}
	if supervisor.Alive(proc.PID) {
		t.Fatal("runtime process still alive after destroy")
	}
	if _, err := os.Stat(filepath.Join(dir, "logs", "dep-1.log")); !os.IsNotExist(err) {
		t.Fatalf("log file not removed: %v", err)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("deployment model cache not removed: %v", err)
	}
	if _, err := os.Stat(cacheRoot); err != nil {
		t.Fatalf("shared cache root should remain: %v", err)
	}
	if !strings.Contains(stdout.String(), "billing continues") {
		t.Fatalf("missing billing warning:\n%s", stdout.String())
	}

	store, err = state.Open(filepath.Join(dir, "iold.db"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.Close()
	got, err := store.Get("dep-1")
	if err != nil {
		t.Fatalf("tombstone missing: %v", err)
	}
	if got.Phase != state.PhaseDestroyed {
		t.Fatalf("phase = %s, want DESTROYED", got.Phase)
	}
}

func TestDestroyPurgeRemovesTombstone(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IOLD_STATE_DIR", dir)
	seedDeployment(t, dir)

	var stdout, stderr bytes.Buffer
	if err := Run([]string{"destroy", "dep-1", "--purge"}, &stdout, &stderr); err != nil {
		t.Fatalf("destroy --purge returned error: %v\n%s", err, stdout.String())
	}
	store, err := state.Open(filepath.Join(dir, "iold.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	if _, err := store.Get("dep-1"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("expected purged record, got %v", err)
	}
}

func TestDestroyRefusesMismatchedModelCacheOwnership(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IOLD_STATE_DIR", dir)
	cacheRoot := filepath.Join(dir, "model-cache")
	t.Setenv("IOLD_MODEL_CACHE", cacheRoot)
	seedDeployment(t, dir)
	cachePath := filepath.Join(cacheRoot, "dep-1")
	if err := prepareModelCache(cachePath, "dep-1", "different-owner"); err != nil {
		t.Fatal(err)
	}
	weightPath := filepath.Join(cachePath, "weights.bin")
	if err := os.WriteFile(weightPath, []byte("must survive"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := Run([]string{"destroy", "dep-1"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "mismatched ownership marker") {
		t.Fatalf("destroy error = %v, want ownership refusal", err)
	}
	if data, readErr := os.ReadFile(weightPath); readErr != nil || string(data) != "must survive" {
		t.Fatalf("unowned cache was modified: data=%q err=%v", data, readErr)
	}
	store, openErr := state.Open(filepath.Join(dir, "iold.db"))
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer store.Close()
	deployment, getErr := store.Get("dep-1")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if deployment.Phase != state.PhaseDestroying {
		t.Fatalf("phase = %s, want DESTROYING after refused cleanup", deployment.Phase)
	}
}

func TestDestroyRequiresTarget(t *testing.T) {
	t.Setenv("IOLD_STATE_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer
	if err := Run([]string{"destroy"}, &stdout, &stderr); err == nil {
		t.Fatal("destroy without target should fail")
	}
	if err := Run([]string{"destroy", "dep-1", "--all"}, &stdout, &stderr); err == nil {
		t.Fatal("destroy with both id and --all should fail")
	}
}

func TestExitCodesAreStable(t *testing.T) {
	t.Setenv("IOLD_STATE_DIR", t.TempDir())
	cases := []struct {
		args []string
		want int
	}{
		{[]string{"models"}, ExitOK},
		{[]string{"bogus-command"}, ExitUsage},
		{[]string{"destroy"}, ExitUsage},
		{[]string{"deploy", "unknown-model"}, ExitNotFound},
		{[]string{"status", "missing-deployment"}, ExitNotFound},
		{[]string{"add", "qwen3.6-35b-a3b"}, ExitNotImplemented},
	}
	for _, tc := range cases {
		var stdout, stderr bytes.Buffer
		err := Run(tc.args, &stdout, &stderr)
		if got := ExitCode(err); got != tc.want {
			t.Fatalf("ExitCode(%v) = %d, want %d (err: %v)", tc.args, got, tc.want, err)
		}
	}
}

func TestOwnedPathsRejectsTraversal(t *testing.T) {
	t.Setenv("IOLD_STATE_DIR", t.TempDir())
	for _, id := range []string{"..", "../other", "a/b", "", ".hidden", "x..y/../z"} {
		if _, err := ownedPaths(id); err == nil {
			t.Fatalf("ownedPaths(%q) accepted unsafe id", id)
		}
	}
	if _, err := ownedPaths("dep-1"); err != nil {
		t.Fatalf("ownedPaths rejected valid id: %v", err)
	}
}
