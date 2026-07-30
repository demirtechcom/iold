package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/demirtechcom/iold/internal/catalog"
	"github.com/demirtechcom/iold/internal/doctor"
	"github.com/demirtechcom/iold/internal/hf"
	"github.com/demirtechcom/iold/internal/operationlock"
	"github.com/demirtechcom/iold/internal/planner"
	"github.com/demirtechcom/iold/internal/preflight"
	"github.com/demirtechcom/iold/internal/state"
	"github.com/demirtechcom/iold/internal/supervisor"
)

// ErrWouldReplace is returned when deploy finds an existing deployment
// and --replace was not given.
var ErrWouldReplace = errors.New("a deployment already exists; pass --replace to replace it")

// deploySpec is a fully resolved deployment request.
type deploySpec struct {
	ID              string // deployment ID and served model name
	Alias           string
	Artifact        string // Hugging Face repository vLLM downloads
	Revision        string // immutable Hugging Face commit SHA
	Port            int
	MaxModelLen     int
	APIKey          string
	CacheDir        string
	Quantization    string
	MinDriverMajor  int
	MinComputeMajor int
	MinVRAMMiB      int
	MinDiskGiB      int
}

// deployDeps carries everything runDeploy touches so component tests
// can substitute the fake vLLM server and a stub runtime command.
type deployDeps struct {
	probes doctor.Probes
	hub    *hf.Client
	http   *http.Client
	// vllmCmd builds the runtime command; tests substitute a stub
	// process here.
	vllmCmd func(spec deploySpec) (string, []string)
	// baseURL returns the local URL health and readiness probes use;
	// tests point it at a fake server.
	baseURL       func(port int) string
	healthTimeout time.Duration
	readyTimeout  time.Duration
	pollInterval  time.Duration
	stopGrace     time.Duration
}

func defaultDeployDeps() deployDeps {
	return deployDeps{
		probes: doctor.NewSystemProbes(),
		hub:    hf.NewClient(os.Getenv("HF_TOKEN")),
		// First inference after a cold start compiles CUDA graphs and
		// can be slow; keep a generous per-request timeout.
		http:    &http.Client{Timeout: 3 * time.Minute},
		vllmCmd: vllmCommand,
		baseURL: func(port int) string { return fmt.Sprintf("http://127.0.0.1:%d", port) },
		// The health window covers the model download, which dominates
		// time-to-ready for large artifacts.
		healthTimeout: 45 * time.Minute,
		readyTimeout:  5 * time.Minute,
		pollInterval:  2 * time.Second,
		stopGrace:     10 * time.Second,
	}
}

// vllmCommand builds the production vLLM server invocation. The API
// key travels via the VLLM_API_KEY environment variable so it never
// appears in `ps` output; HF_TOKEN reaches the download step through
// the inherited environment.
func vllmCommand(spec deploySpec) (string, []string) {
	bin := os.Getenv("IOLD_VLLM_BIN")
	if bin == "" {
		bin = "vllm"
	}
	args := []string{
		"serve", spec.Artifact,
		"--host", "0.0.0.0", // the RunPod proxy reaches the pod port directly (D-019)
		"--port", strconv.Itoa(spec.Port),
		"--served-model-name", spec.ID,
	}
	if spec.MaxModelLen > 0 {
		args = append(args, "--max-model-len", strconv.Itoa(spec.MaxModelLen))
	}
	if spec.Revision != "" {
		args = append(args, "--revision", spec.Revision)
	}
	if spec.CacheDir != "" {
		args = append(args, "--download-dir", spec.CacheDir)
	}
	return bin, args
}

// runDeploy implements `iold deploy <model> [--replace]` (M3-04): the
// model may be a catalog alias (verified fast path) or any Hugging
// Face model ID gated by the planner (D-017).
func runDeploy(args []string, deps deployDeps, stdout io.Writer) error {
	usage := fmt.Errorf("%w: iold deploy <catalog-alias|hf-model> [--replace]", ErrUsage)
	replace := false
	var ref string
	for _, arg := range args {
		switch {
		case arg == "--replace":
			replace = true
		case ref == "" && !strings.HasPrefix(arg, "-"):
			ref = arg
		default:
			return usage
		}
	}
	if ref == "" {
		return usage
	}

	lifecycleLock, err := operationlock.Acquire(operationLockPath())
	if err != nil {
		return err
	}
	defer lifecycleLock.Close()

	store, err := state.Open(stateDBPath())
	if err != nil {
		return err
	}
	defer store.Close()
	if err := recoverInterruptedDeployments(store); err != nil {
		return err
	}

	// Replace semantics (D-006). v0 replaces with downtime: the old
	// runtime is destroyed before the new one starts, because a single
	// GPU rarely fits both. Transactional keep-old-until-new-is-ready
	// is deferred.
	existing, err := store.List()
	if err != nil {
		return err
	}
	var active []state.Deployment
	for _, old := range existing {
		if old.Phase == state.PhaseDestroyed {
			continue
		}
		if !replace {
			return fmt.Errorf("%w (existing: %s in phase %s): %w", ErrWouldReplace, old.ID, old.Phase, state.ErrConflict)
		}
		active = append(active, old)
	}

	spec, err := resolveSpec(deps, ref)
	if err != nil {
		return err
	}
	runtimeBinary, _ := deps.vllmCmd(spec)
	releasable := make([]int, 0, len(active))
	for _, old := range active {
		releasable = append(releasable, old.Port)
	}
	checked, err := preflight.Run(preflight.Spec{
		Probes:          deps.probes,
		RuntimeBinary:   runtimeBinary,
		CacheDir:        spec.CacheDir,
		Endpoint:        deps.hub.BaseURL,
		MinDriverMajor:  spec.MinDriverMajor,
		MinComputeMajor: spec.MinComputeMajor,
		MinVRAMMiB:      spec.MinVRAMMiB,
		MinDiskGiB:      spec.MinDiskGiB,
		PreferredPort:   spec.Port,
		ReleasablePorts: releasable,
	})
	if err != nil {
		return err
	}
	spec.Port = checked.Port

	for _, old := range active {
		fmt.Fprintf(stdout, "Replacing deployment %s (downtime until the new model is ready).\n", old.ID)
		if err := destroyOne(store, old, true, stdout); err != nil {
			return fmt.Errorf("replace: destroy %s: %w", old.ID, err)
		}
	}

	// Clear a tombstone that would collide with the new ID.
	if old, err := store.Get(spec.ID); err == nil && old.Phase == state.PhaseDestroyed {
		if err := store.Delete(spec.ID); err != nil {
			return err
		}
	}

	deployment, err := store.Create(state.Deployment{
		ID:               spec.ID,
		Alias:            spec.Alias,
		Artifact:         spec.Artifact,
		ArtifactRevision: spec.Revision,
		Port:             spec.Port,
		IdempotencyKey:   randomHex(16),
	})
	if err != nil {
		return err
	}
	return executeDeploy(store, deployment, spec, deps, stdout)
}

// resolveSpec turns a model reference into a runnable spec without any
// side effects, so unsupported requests fail before anything is
// created (docs/TESTING.md failure matrix).
func resolveSpec(deps deployDeps, ref string) (deploySpec, error) {
	if entry, err := catalog.Get(ref); err == nil {
		revision := entry.ArtifactRevision
		if revision == "" || strings.HasPrefix(revision, "MOCK_") {
			model, err := deps.hub.GetModel(context.Background(), entry.Artifact)
			if err != nil {
				return deploySpec{}, err
			}
			if err := validateRevision(model.SHA); err != nil {
				return deploySpec{}, fmt.Errorf("catalog revision for %s: %w", entry.Artifact, err)
			}
			revision = model.SHA
		} else if err := validateRevision(revision); err != nil {
			return deploySpec{}, fmt.Errorf("catalog revision for %s: %w", entry.Artifact, err)
		}
		cacheDir, err := deploymentModelCacheDir(entry.Alias)
		if err != nil {
			return deploySpec{}, err
		}
		return deploySpec{
			ID:              entry.Alias,
			Alias:           entry.Alias,
			Artifact:        entry.Artifact,
			Revision:        revision,
			Port:            entry.DefaultPort,
			MaxModelLen:     entry.MaxModelLen,
			CacheDir:        cacheDir,
			Quantization:    entry.Quantization,
			MinDriverMajor:  570,
			MinComputeMajor: minComputeForQuantization(entry.Quantization),
			MinVRAMMiB:      entry.MinimumVRAMGiB * 1024,
			MinDiskGiB:      100,
		}, nil
	}
	if !strings.Contains(ref, "/") {
		return deploySpec{}, fmt.Errorf("%w: %s (not a catalog alias; Hugging Face IDs look like org/model)", catalog.ErrUnknownModel, ref)
	}

	// Planner gate (D-017): reject models that cannot fit before any
	// download or state is created.
	vramMiB, err := primaryVRAMMiB(deps.probes)
	if err != nil {
		return deploySpec{}, err
	}
	ctx := context.Background()
	model, err := deps.hub.GetModel(ctx, ref)
	if err != nil {
		return deploySpec{}, err
	}
	if model.Gated.IsGated && deps.hub.Token == "" {
		return deploySpec{}, fmt.Errorf("%w: %s", hf.ErrAuthRequired, ref)
	}
	if err := validateRevision(model.SHA); err != nil {
		return deploySpec{}, fmt.Errorf("resolve immutable revision for %s: %w", ref, err)
	}
	cfg, err := deps.hub.GetConfigAtRevision(ctx, ref, model.SHA)
	if err != nil && !errors.Is(err, hf.ErrNotFound) {
		return deploySpec{}, err
	}
	est, err := planner.EstimateModel(model, cfg, planner.DefaultContextTokens)
	if err != nil {
		return deploySpec{}, err
	}
	if planner.Assess(est, vramMiB) == planner.VerdictTooBig {
		return deploySpec{}, fmt.Errorf("%w: %s needs ~%.1f GiB but this GPU has %d MiB; run `iold plan %s` for fitting variants",
			doctor.ErrChecksFailed, ref, planner.GiB(est.TotalBytes), vramMiB, ref)
	}
	maxLen := planner.DefaultContextTokens
	if cfg.MaxPositionEmbeddings > 0 && cfg.MaxPositionEmbeddings < maxLen {
		maxLen = cfg.MaxPositionEmbeddings
	}
	id := deploymentID(ref)
	cacheDir, err := deploymentModelCacheDir(id)
	if err != nil {
		return deploySpec{}, err
	}
	quantization := planner.DetectQuantization(model, cfg)
	return deploySpec{
		ID:              id,
		Alias:           ref,
		Artifact:        ref,
		Revision:        model.SHA,
		Port:            8000,
		MaxModelLen:     maxLen,
		CacheDir:        cacheDir,
		Quantization:    quantization,
		MinDriverMajor:  570,
		MinComputeMajor: minComputeForQuantization(quantization),
		MinVRAMMiB:      requiredVRAMMiB(est.TotalBytes),
		MinDiskGiB:      requiredDiskGiB(est.WeightBytes),
	}, nil
}

func primaryVRAMMiB(probes doctor.Probes) (int, error) {
	gpus, err := probes.GPUs()
	if err != nil || len(gpus) == 0 {
		return 0, fmt.Errorf("%w: no NVIDIA GPU detected (%v)", doctor.ErrChecksFailed, err)
	}
	return gpus[0].VRAMMiB, nil
}

var revisionPattern = regexp.MustCompile(`^[a-fA-F0-9]{40,64}$`)

func validateRevision(revision string) error {
	if !revisionPattern.MatchString(revision) {
		return fmt.Errorf("%w: got %q", hf.ErrNoRevision, revision)
	}
	return nil
}

func minComputeForQuantization(quantization string) int {
	switch strings.ToLower(quantization) {
	case "nvfp4", "mxfp4", "fp4":
		return 10
	default:
		return 0
	}
}

func requiredVRAMMiB(totalBytes int64) int {
	// vLLM reserves at most 90% of physical VRAM by default.
	return int((totalBytes*10/9 + (1 << 20) - 1) / (1 << 20))
}

func requiredDiskGiB(weightBytes int64) int {
	// Add 20% download slack plus fixed tokenizer/metadata headroom.
	value := int((weightBytes*6/5+(1<<30)-1)/(1<<30)) + 5
	if value < 10 {
		return 10
	}
	return value
}

var idSanitizer = regexp.MustCompile(`[^a-z0-9._-]+`)

// deploymentID derives a safe single-segment ID from a Hugging Face
// reference, e.g. "Qwen/Qwen2.5-7B-Instruct" -> "qwen2.5-7b-instruct".
func deploymentID(ref string) string {
	if _, after, found := strings.Cut(ref, "/"); found {
		ref = after
	}
	id := idSanitizer.ReplaceAllString(strings.ToLower(ref), "-")
	id = strings.Trim(id, "-._")
	if len(id) > 64 {
		id = id[:64]
	}
	if id == "" || validateID(id) != nil {
		id = "deployment-" + randomHex(4)
	}
	return id
}

// executeDeploy drives the state machine from REQUESTED to
// UNREGISTERED_HEALTHY. Every failure marks the record FAILED with an
// actionable reason and stops any process this run started.
func executeDeploy(store *state.Store, deployment state.Deployment, spec deploySpec, deps deployDeps, stdout io.Writer) error {
	var proc *supervisor.Process
	fail := func(from state.Phase, cause error) error {
		if proc != nil {
			if stopErr := supervisor.Stop(*proc, deps.stopGrace); stopErr != nil {
				fmt.Fprintf(stdout, "warning: could not stop runtime pid %d: %v\n", proc.PID, stopErr)
			}
		}
		if err := store.Transition(deployment.ID, from, state.PhaseFailed, cause.Error()); err != nil {
			fmt.Fprintf(stdout, "warning: could not record failure: %v\n", err)
		}
		return fmt.Errorf("deploy %s: %w (see `iold logs %s`)", deployment.ID, cause, deployment.ID)
	}

	// VALIDATING: preflight already selected the port; create only reversible
	// per-deployment key material here.
	if err := store.Transition(deployment.ID, state.PhaseRequested, state.PhaseValidating, ""); err != nil {
		return err
	}
	spec.APIKey = "iold-" + randomHex(32)
	if err := writeAPIKey(deployment.ID, spec.APIKey); err != nil {
		return fail(state.PhaseValidating, err)
	}

	// DOWNLOADING/STARTING: vLLM downloads the artifact during startup,
	// so the two phases share one process launch.
	if err := store.Transition(deployment.ID, state.PhaseValidating, state.PhaseDownloading, ""); err != nil {
		return err
	}
	logFile, err := logPath(deployment.ID)
	if err != nil {
		return fail(state.PhaseDownloading, err)
	}
	command, cmdArgs := deps.vllmCmd(spec)
	if err := store.SetRuntimeIntent(deployment.ID, spec.Port, runtimeCommandString(command, cmdArgs)); err != nil {
		return fail(state.PhaseDownloading, err)
	}
	if err := store.Transition(deployment.ID, state.PhaseDownloading, state.PhaseStarting, ""); err != nil {
		return fail(state.PhaseDownloading, err)
	}
	started, err := supervisor.Start(supervisor.Spec{
		Command: command,
		Args:    cmdArgs,
		Env: []string{
			"VLLM_API_KEY=" + spec.APIKey,
			"HF_HOME=" + spec.CacheDir,
			"HUGGINGFACE_HUB_CACHE=" + filepath.Join(spec.CacheDir, "hub"),
		},
		LogPath: logFile,
	})
	if err != nil {
		return fail(state.PhaseStarting, err)
	}
	proc = &started
	if err := store.SetRuntime(deployment.ID, started.PID, spec.Port, started.Command,
		started.StartedAt, started.StartToken); err != nil {
		return fail(state.PhaseStarting, err)
	}
	fmt.Fprintf(stdout, "%s: runtime started (pid %d, port %d); waiting for health...\n", deployment.ID, started.PID, spec.Port)

	// HEALTHY: /health answers 200 while the process is still ours.
	base := deps.baseURL(spec.Port)
	if err := waitHealthy(deps, base, *proc); err != nil {
		return fail(state.PhaseStarting, err)
	}
	if err := store.Transition(deployment.ID, state.PhaseStarting, state.PhaseHealthy, ""); err != nil {
		return fail(state.PhaseStarting, err)
	}

	// Readiness (docs/ARCHITECTURE.md §7): a healthy process is not a ready
	// deployment until /v1/models lists the model and one deterministic
	// inference succeeds.
	if err := checkReadiness(deps, base, spec); err != nil {
		return fail(state.PhaseHealthy, err)
	}

	// Gateway registration is M4; record the honest split state.
	if err := store.Transition(deployment.ID, state.PhaseHealthy, state.PhaseRegistering, ""); err != nil {
		return fail(state.PhaseHealthy, err)
	}
	if err := store.Transition(deployment.ID, state.PhaseRegistering, state.PhaseUnregisteredHealthy,
		"gateway registration not implemented yet (M4)"); err != nil {
		return fail(state.PhaseRegistering, err)
	}

	printDeploySummary(stdout, deployment.ID, spec)
	return nil
}

func runtimeCommandString(command string, args []string) string {
	return strings.Join(append([]string{command}, args...), " ")
}

func waitHealthy(deps deployDeps, base string, proc supervisor.Process) error {
	deadline := time.Now().Add(deps.healthTimeout)
	for {
		if supervisor.Reconcile(proc) != supervisor.StatusRunning {
			return errors.New("runtime process exited during startup")
		}
		resp, err := deps.http.Get(base + "/health")
		if err == nil {
			status := resp.StatusCode
			resp.Body.Close()
			if status == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("runtime did not become healthy within %s", deps.healthTimeout)
		}
		time.Sleep(deps.pollInterval)
	}
}

func checkReadiness(deps deployDeps, base string, spec deploySpec) error {
	deadline := time.Now().Add(deps.readyTimeout)
	for {
		err := readinessProbe(deps, base, spec)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("readiness checks failed: %w", err)
		}
		time.Sleep(deps.pollInterval)
	}
}

func readinessProbe(deps deployDeps, base string, spec deploySpec) error {
	models, err := authedGet(deps.http, base+"/v1/models", spec.APIKey)
	if err != nil {
		return fmt.Errorf("list models: %w", err)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(models, &list); err != nil {
		return fmt.Errorf("parse model list: %w", err)
	}
	found := false
	for _, m := range list.Data {
		if m.ID == spec.ID {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("served model %q missing from /v1/models", spec.ID)
	}

	body, _ := json.Marshal(map[string]any{
		"model":       spec.ID,
		"messages":    []map[string]string{{"role": "user", "content": "Reply with one word."}},
		"max_tokens":  8,
		"temperature": 0,
	})
	req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+spec.APIKey)
	resp, err := deps.http.Do(req)
	if err != nil {
		return fmt.Errorf("inference check: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("inference check: status %d", resp.StatusCode)
	}
	var chat struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &chat); err != nil {
		return fmt.Errorf("parse inference response: %w", err)
	}
	if len(chat.Choices) == 0 || chat.Choices[0].Message.Content == "" {
		return errors.New("inference check returned no content")
	}
	return nil
}

func authedGet(client *http.Client, url, apiKey string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// writeAPIKey stores the per-deployment key under the deployment's
// owned directory with restrictive permissions; destroy removes it
// with the rest of the deployment data.
func writeAPIKey(id, key string) error {
	paths, err := ownedPaths(id)
	if err != nil {
		return err
	}
	dir := paths[1] // deployments/<id>
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "api-key")
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// endpointURL resolves the public endpoint (M4-01): the RunPod proxy
// URL when running on a Pod, otherwise the local address.
func endpointURL(port int) string {
	if pod := os.Getenv("RUNPOD_POD_ID"); pod != "" {
		return fmt.Sprintf("https://%s-%d.proxy.runpod.net", pod, port)
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

func printDeploySummary(stdout io.Writer, id string, spec deploySpec) {
	endpoint := endpointURL(spec.Port)
	fmt.Fprintf(stdout, `%s: READY to serve (registration pending, see status)

  Endpoint:   %s/v1
  Model name: %s
  API key:    %s
  Key file:   %s

Test it:
  curl %s/v1/chat/completions \
    -H "Authorization: Bearer %s" \
    -H "Content-Type: application/json" \
    -d '{"model": "%s", "messages": [{"role": "user", "content": "Merhaba"}]}'

Notes:
  - Requests without the API key are rejected by vLLM.
  - Gateway registration (Open WebUI/kagent) is not automated yet; add
    the endpoint above as an OpenAI-compatible connection manually.
  - The RunPod proxy closes connections after ~100 seconds; long
    streaming responses may be cut (measurement pending).
`, id, endpoint, id, spec.APIKey, filepath.Join(stateDir(), "deployments", id, "api-key"),
		endpoint, spec.APIKey, id)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(buf)
}
