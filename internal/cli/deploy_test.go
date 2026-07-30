package cli

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/demirtechcom/iold/internal/doctor"
	"github.com/demirtechcom/iold/internal/fakes"
	"github.com/demirtechcom/iold/internal/state"
	"github.com/demirtechcom/iold/internal/supervisor"
)

// testDeployDeps wires deploy to a fake vLLM HTTP server and a stub
// long-running process, with timeouts scaled for tests.
func testDeployDeps(t *testing.T, vllm *fakes.VLLM) deployDeps {
	t.Helper()
	hub := fakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/unsloth/Qwen3.6-35B-A3B-NVFP4-Fast" {
			w.Write([]byte(`{"id":"unsloth/Qwen3.6-35B-A3B-NVFP4-Fast","sha":"` + testRevision + `","gated":false}`))
			return
		}
		http.NotFound(w, r)
	})
	return deployDeps{
		probes:        planProbes(96 * 1024),
		hub:           hub,
		http:          &http.Client{Timeout: 5 * time.Second},
		vllmCmd:       func(deploySpec) (string, []string) { return "sleep", []string{"300"} },
		baseURL:       func(int) string { return vllm.URL },
		healthTimeout: 3 * time.Second,
		readyTimeout:  3 * time.Second,
		pollInterval:  20 * time.Millisecond,
		stopGrace:     2 * time.Second,
	}
}

func stopDeploymentProcess(t *testing.T, dbPath, id string) {
	t.Helper()
	store, err := state.Open(dbPath)
	if err != nil {
		return
	}
	defer store.Close()
	if d, err := store.Get(id); err == nil && d.PID > 0 {
		proc := deploymentProcess(d)
		if supervisor.Reconcile(proc) == supervisor.StatusRunning {
			_ = supervisor.Stop(proc, 2*time.Second)
		}
	}
}

func TestDeployCatalogModelReachesUnregisteredHealthy(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("IOLD_STATE_DIR", stateDir)
	vllm := fakes.NewVLLM(fakes.VLLMOptions{Model: "qwen3.6-35b-a3b", HealthyAfter: 2})
	defer vllm.Close()
	t.Cleanup(func() { stopDeploymentProcess(t, filepath.Join(stateDir, "iold.db"), "qwen3.6-35b-a3b") })

	var stdout bytes.Buffer
	if err := runDeploy([]string{"qwen3.6-35b-a3b"}, testDeployDeps(t, vllm), &stdout); err != nil {
		t.Fatalf("runDeploy: %v\n%s", err, stdout.String())
	}

	store, err := state.Open(filepath.Join(stateDir, "iold.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	deployment, err := store.Get("qwen3.6-35b-a3b")
	if err != nil {
		t.Fatal(err)
	}
	if deployment.Phase != state.PhaseUnregisteredHealthy {
		t.Errorf("phase = %s, want UNREGISTERED_HEALTHY", deployment.Phase)
	}
	if deployment.ArtifactRevision != testRevision {
		t.Errorf("revision = %q, want immutable SHA %q", deployment.ArtifactRevision, testRevision)
	}
	if deployment.PID <= 0 || !supervisor.Alive(deployment.PID) {
		t.Errorf("runtime pid %d not alive", deployment.PID)
	}

	keyPath := filepath.Join(stateDir, "deployments", "qwen3.6-35b-a3b", "api-key")
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("api key file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("api key mode = %v, want 0600", info.Mode().Perm())
	}
	key, _ := os.ReadFile(keyPath)
	out := stdout.String()
	if !strings.Contains(out, strings.TrimSpace(string(key))) {
		t.Error("summary does not show the generated API key")
	}
	if !strings.Contains(out, "http://127.0.0.1:") {
		t.Errorf("summary missing endpoint URL:\n%s", out)
	}
	if vllm.HealthCalls() < 3 {
		t.Errorf("health polled %d times, want at least 3 (slow start)", vllm.HealthCalls())
	}
}

func TestDeployFailedLoadMarksFailedAndStopsRuntime(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("IOLD_STATE_DIR", stateDir)
	vllm := fakes.NewVLLM(fakes.VLLMOptions{FailLoad: true})
	defer vllm.Close()
	deps := testDeployDeps(t, vllm)
	deps.healthTimeout = 200 * time.Millisecond

	var stdout bytes.Buffer
	err := runDeploy([]string{"qwen3.6-35b-a3b"}, deps, &stdout)
	if err == nil {
		t.Fatal("expected failure")
	}

	store, err := state.Open(filepath.Join(stateDir, "iold.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	deployment, err := store.Get("qwen3.6-35b-a3b")
	if err != nil {
		t.Fatal(err)
	}
	if deployment.Phase != state.PhaseFailed {
		t.Errorf("phase = %s, want FAILED", deployment.Phase)
	}
	if deployment.FailureReason == "" {
		t.Error("failure reason not recorded")
	}
	if deployment.PID > 0 && supervisor.Alive(deployment.PID) {
		proc := deploymentProcess(deployment)
		if supervisor.Reconcile(proc) == supervisor.StatusRunning {
			t.Error("runtime process leaked after failed deploy")
			_ = supervisor.Stop(proc, 2*time.Second)
		}
	}
}

func TestDeployRefusesToReplaceWithoutFlag(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("IOLD_STATE_DIR", stateDir)
	store, err := state.Open(filepath.Join(stateDir, "iold.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(state.Deployment{ID: "old-model", Alias: "old", Artifact: "org/old", IdempotencyKey: "k"}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	vllm := fakes.NewVLLM(fakes.VLLMOptions{Model: "qwen3.6-35b-a3b"})
	defer vllm.Close()
	var stdout bytes.Buffer
	err = runDeploy([]string{"qwen3.6-35b-a3b"}, testDeployDeps(t, vllm), &stdout)
	if !errors.Is(err, ErrWouldReplace) {
		t.Fatalf("err = %v, want ErrWouldReplace", err)
	}
	if ExitCode(err) != ExitConflict {
		t.Errorf("exit code = %d, want %d", ExitCode(err), ExitConflict)
	}
}

func TestDeployReplaceDestroysOldDeployment(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("IOLD_STATE_DIR", stateDir)
	dbPath := filepath.Join(stateDir, "iold.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(state.Deployment{ID: "old-model", Alias: "old", Artifact: "org/old", IdempotencyKey: "k"}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	vllm := fakes.NewVLLM(fakes.VLLMOptions{Model: "qwen3.6-35b-a3b"})
	defer vllm.Close()
	t.Cleanup(func() { stopDeploymentProcess(t, dbPath, "qwen3.6-35b-a3b") })

	var stdout bytes.Buffer
	if err := runDeploy([]string{"qwen3.6-35b-a3b", "--replace"}, testDeployDeps(t, vllm), &stdout); err != nil {
		t.Fatalf("runDeploy --replace: %v\n%s", err, stdout.String())
	}

	store, err = state.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Get("old-model"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("old deployment still present: %v", err)
	}
	if d, err := store.Get("qwen3.6-35b-a3b"); err != nil || d.Phase != state.PhaseUnregisteredHealthy {
		t.Errorf("new deployment = %+v, err %v", d, err)
	}
}

func TestDeployHFModelTooBigFailsBeforeAnySideEffect(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("IOLD_STATE_DIR", stateDir)
	hub := fakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/org/huge-70b":
			w.Write([]byte(`{"id":"org/huge-70b","sha":"` + testRevision + `","gated":false,"safetensors":{"parameters":{"BF16":70000000000},"total":70000000000}}`))
		case "/org/huge-70b/raw/" + testRevision + "/config.json":
			w.Write([]byte(`{"model_type":"llama","num_hidden_layers":80,"num_attention_heads":64,"num_key_value_heads":8,"hidden_size":8192}`))
		default:
			http.NotFound(w, r)
		}
	})
	vllm := fakes.NewVLLM(fakes.VLLMOptions{})
	defer vllm.Close()
	deps := testDeployDeps(t, vllm)
	deps.hub = hub

	var stdout bytes.Buffer
	err := runDeploy([]string{"org/huge-70b"}, deps, &stdout)
	if !errors.Is(err, doctor.ErrChecksFailed) {
		t.Fatalf("err = %v, want ErrChecksFailed", err)
	}

	store, err := state.Open(filepath.Join(stateDir, "iold.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	deployments, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(deployments) != 0 {
		t.Errorf("side effects before validation: %+v", deployments)
	}
}

func TestDeployHFModelResolvesViaPlannerGate(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("IOLD_STATE_DIR", stateDir)
	hub := fakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/Qwen/Small-7B":
			w.Write([]byte(`{"id":"Qwen/Small-7B","sha":"` + testRevision + `","gated":false,"safetensors":{"parameters":{"BF16":7000000000},"total":7000000000}}`))
		case "/Qwen/Small-7B/raw/" + testRevision + "/config.json":
			w.Write([]byte(`{"model_type":"qwen3","num_hidden_layers":32,"num_attention_heads":32,"num_key_value_heads":8,"hidden_size":4096,"max_position_embeddings":32768}`))
		default:
			http.NotFound(w, r)
		}
	})
	vllm := fakes.NewVLLM(fakes.VLLMOptions{Model: "small-7b"})
	defer vllm.Close()
	deps := testDeployDeps(t, vllm)
	deps.hub = hub
	t.Cleanup(func() { stopDeploymentProcess(t, filepath.Join(stateDir, "iold.db"), "small-7b") })

	var stdout bytes.Buffer
	if err := runDeploy([]string{"Qwen/Small-7B"}, deps, &stdout); err != nil {
		t.Fatalf("runDeploy: %v\n%s", err, stdout.String())
	}
	store, err := state.Open(filepath.Join(stateDir, "iold.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	d, err := store.Get("small-7b")
	if err != nil {
		t.Fatal(err)
	}
	if d.Phase != state.PhaseUnregisteredHealthy || d.Artifact != "Qwen/Small-7B" || d.ArtifactRevision != testRevision {
		t.Errorf("deployment = %+v", d)
	}
}

func TestDeployCatalogRunsSharedHardwarePreflight(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("IOLD_STATE_DIR", stateDir)
	vllm := fakes.NewVLLM(fakes.VLLMOptions{Model: "qwen3.6-35b-a3b"})
	defer vllm.Close()
	deps := testDeployDeps(t, vllm)
	deps.probes = fakeProbes{
		gpus: []doctor.GPU{{
			Name: "NVIDIA H100", DriverVersion: "575.51.03",
			VRAMMiB: 96 * 1024, ComputeCapMajor: 9,
		}},
		diskFree: 500 << 30,
	}
	err := runDeploy([]string{"qwen3.6-35b-a3b"}, deps, &bytes.Buffer{})
	if !errors.Is(err, doctor.ErrChecksFailed) {
		t.Fatalf("err = %v, want hardware preflight failure", err)
	}
	store, openErr := state.Open(filepath.Join(stateDir, "iold.db"))
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer store.Close()
	deployments, listErr := store.List()
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(deployments) != 0 {
		t.Fatalf("preflight created state: %+v", deployments)
	}
}

func TestDeployPassesOwnedCacheToRuntime(t *testing.T) {
	stateDir := t.TempDir()
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	t.Setenv("IOLD_STATE_DIR", stateDir)
	t.Setenv("IOLD_MODEL_CACHE", cacheRoot)
	vllm := fakes.NewVLLM(fakes.VLLMOptions{Model: "qwen3.6-35b-a3b"})
	defer vllm.Close()
	deps := testDeployDeps(t, vllm)
	deps.vllmCmd = func(deploySpec) (string, []string) {
		return "sh", []string{"-c", `echo "HF_HOME=$HF_HOME"; while true; do sleep 0.1; done`}
	}
	t.Cleanup(func() { stopDeploymentProcess(t, filepath.Join(stateDir, "iold.db"), "qwen3.6-35b-a3b") })
	if err := runDeploy([]string{"qwen3.6-35b-a3b"}, deps, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := "HF_HOME=" + filepath.Join(cacheRoot, "qwen3.6-35b-a3b")
	logFile := filepath.Join(stateDir, "logs", "qwen3.6-35b-a3b.log")
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(logFile)
		if err == nil && strings.Contains(string(data), want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runtime log did not contain %q: %s", want, data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	persistedCache := filepath.Join(cacheRoot, "qwen3.6-35b-a3b")
	t.Setenv("IOLD_MODEL_CACHE", filepath.Join(t.TempDir(), "different-cache"))
	if err := Run([]string{"destroy", "qwen3.6-35b-a3b"}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("destroy with changed cache environment: %v", err)
	}
	if _, err := os.Stat(persistedCache); !os.IsNotExist(err) {
		t.Fatalf("persisted deployment cache was not removed: %v", err)
	}
}

func TestVLLMCommandPinsRevisionAndDownloadDirectory(t *testing.T) {
	_, args := vllmCommand(deploySpec{
		ID: "model", Artifact: "org/model", Revision: testRevision,
		Port: 8000, CacheDir: "/workspace/.iold/models/model",
	})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--revision "+testRevision) ||
		!strings.Contains(joined, "--download-dir /workspace/.iold/models/model") {
		t.Fatalf("vLLM args are not pinned to revision/cache: %v", args)
	}
}

func TestConcurrentDeploysAreSerializedByLifecycleLock(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("IOLD_STATE_DIR", stateDir)
	hub := fakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/org/model-a", "/api/models/org/model-b":
			id := strings.TrimPrefix(r.URL.Path, "/api/models/")
			fmt.Fprintf(w, `{"id":%q,"sha":%q,"gated":false,"safetensors":{"parameters":{"BF16":1000000000},"total":1000000000}}`, id, testRevision)
		case "/org/model-a/raw/" + testRevision + "/config.json",
			"/org/model-b/raw/" + testRevision + "/config.json":
			w.Write([]byte(`{"model_type":"llama","num_hidden_layers":8,"num_attention_heads":8,"num_key_value_heads":2,"hidden_size":1024}`))
		default:
			http.NotFound(w, r)
		}
	})
	vllmA := fakes.NewVLLM(fakes.VLLMOptions{Model: "model-a"})
	defer vllmA.Close()
	vllmB := fakes.NewVLLM(fakes.VLLMOptions{Model: "model-b"})
	defer vllmB.Close()
	depsA := testDeployDeps(t, vllmA)
	depsB := testDeployDeps(t, vllmB)
	depsA.hub, depsB.hub = hub, hub

	errs := make(chan error, 2)
	go func() { errs <- runDeploy([]string{"org/model-a"}, depsA, &bytes.Buffer{}) }()
	go func() { errs <- runDeploy([]string{"org/model-b"}, depsB, &bytes.Buffer{}) }()
	first, second := <-errs, <-errs
	var successes, conflicts int
	for _, err := range []error{first, second} {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrWouldReplace):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent deploy result: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d, want one each", successes, conflicts)
	}

	store, err := state.Open(filepath.Join(stateDir, "iold.db"))
	if err != nil {
		t.Fatal(err)
	}
	deployments, err := store.List()
	store.Close()
	if err != nil || len(deployments) != 1 {
		t.Fatalf("deployments=%+v err=%v", deployments, err)
	}
	proc := deploymentProcess(deployments[0])
	if supervisor.Reconcile(proc) == supervisor.StatusRunning {
		_ = supervisor.Stop(proc, time.Second)
	}
}

func TestDeploymentIDDerivation(t *testing.T) {
	cases := map[string]string{
		"Qwen/Qwen2.5-7B-Instruct": "qwen2.5-7b-instruct",
		"org/Weird  Name!!":        "weird-name",
	}
	for ref, want := range cases {
		if got := deploymentID(ref); got != want {
			t.Errorf("deploymentID(%q) = %q, want %q", ref, got, want)
		}
	}
}
