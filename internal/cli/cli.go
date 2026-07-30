package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/demirtechcom/iold/internal/catalog"
	"github.com/demirtechcom/iold/internal/doctor"
	"github.com/demirtechcom/iold/internal/hf"
	"github.com/demirtechcom/iold/internal/operationlock"
	"github.com/demirtechcom/iold/internal/redact"
	"github.com/demirtechcom/iold/internal/state"
	"github.com/demirtechcom/iold/internal/supervisor"
)

var ErrNotImplemented = errors.New("command is not implemented yet")

// Version is stamped by the release build via
// -ldflags "-X github.com/demirtechcom/iold/internal/cli.Version=v1.2.3".
var Version = "dev"

func Run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stdout)
		return nil
	}

	switch args[0] {
	case "help", "--help", "-h":
		printUsage(stdout)
		return nil
	case "version", "--version":
		fmt.Fprintln(stdout, "iold "+Version)
		return nil
	case "models":
		models := catalog.List()
		sort.Slice(models, func(i, j int) bool { return models[i].Alias < models[j].Alias })
		for _, model := range models {
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", model.Alias, model.Quantization, model.Artifact)
		}
		return nil
	case "deploy":
		return runDeploy(args[1:], defaultDeployDeps(), stdout)
	case "add":
		return fmt.Errorf("%w: add (multi-model colocation) arrives in milestone M6; use deploy", ErrNotImplemented)
	case "plan":
		return runPlan(args[1:], doctor.NewSystemProbes(), hf.NewClient(os.Getenv("HF_TOKEN")), stdout)
	case "doctor":
		return runDoctor(doctor.NewSystemProbes(), stdout)
	case "status":
		return runStatus(args[1:], stdout)
	case "logs":
		return runLogs(args[1:], stdout)
	case "destroy":
		return runDestroy(args[1:], stdout)
	default:
		fmt.Fprintf(stderr, "unknown command: %s\n", args[0])
		printUsage(stderr)
		return fmt.Errorf("%w: unknown command %q", ErrUsage, args[0])
	}
}

func runDoctor(probes doctor.Probes, stdout io.Writer) error {
	req := doctor.DefaultRequirements(modelCacheDir())
	for _, model := range catalog.List() {
		if model.MinimumVRAMGiB > req.MinVRAMGiB {
			req.MinVRAMGiB = model.MinimumVRAMGiB
		}
	}
	report := doctor.Run(probes, req)
	for _, check := range report.Checks {
		fmt.Fprintf(stdout, "%-4s %-16s %s\n", check.Status, check.Name, check.Detail)
	}
	if !report.OK() {
		return fmt.Errorf("%w: this host cannot run IOLD deployments", doctor.ErrChecksFailed)
	}
	fmt.Fprintln(stdout, "All checks passed.")
	return nil
}

func runStatus(args []string, stdout io.Writer) error {
	var id string
	asJSON := false
	for _, arg := range args {
		switch {
		case arg == "--json":
			asJSON = true
		case id == "":
			id = arg
		default:
			return fmt.Errorf("%w: iold status [deployment] [--json]", ErrUsage)
		}
	}

	// Reconcile only when no deploy/destroy owns the lifecycle lock. A status
	// call remains non-blocking while a long model start is legitimately in
	// progress.
	lifecycleLock, lockErr := operationlock.TryAcquire(operationLockPath())
	if lockErr != nil && !errors.Is(lockErr, operationlock.ErrBusy) {
		return lockErr
	}
	if lifecycleLock != nil {
		defer lifecycleLock.Close()
	}

	store, err := state.Open(stateDBPath())
	if err != nil {
		return err
	}
	defer store.Close()
	if lifecycleLock != nil {
		if err := recoverInterruptedDeployments(store); err != nil {
			return err
		}
	}

	var deployments []state.Deployment
	if id != "" {
		deployment, err := store.Get(id)
		if err != nil {
			return err
		}
		deployments = []state.Deployment{deployment}
	} else if deployments, err = store.List(); err != nil {
		return err
	}

	if asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if deployments == nil {
			deployments = []state.Deployment{}
		}
		return encoder.Encode(deployments)
	}

	if len(deployments) == 0 {
		fmt.Fprintln(stdout, "No deployments. Run `iold deploy <catalog-model>` to create one.")
		return nil
	}
	writer := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tALIAS\tPHASE\tPORT\tPID\tUPDATED")
	for _, d := range deployments {
		detail := string(d.Phase)
		if d.FailureReason != "" {
			detail += " (" + d.FailureReason + ")"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%d\t%s\n",
			d.ID, d.Alias, detail, d.Port, d.PID, d.UpdatedAt.UTC().Format(time.RFC3339))
	}
	return writer.Flush()
}

func runLogs(args []string, stdout io.Writer) error {
	if len(args) > 1 {
		return fmt.Errorf("%w: iold logs [deployment]", ErrUsage)
	}
	store, err := state.Open(stateDBPath())
	if err != nil {
		return err
	}
	defer store.Close()

	deployment, err := resolveTarget(store, args)
	if err != nil {
		return err
	}
	path, err := logPath(deployment.ID)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("no logs for deployment %s: %w", deployment.ID, err)
	}
	defer file.Close()
	return redact.Copy(stdout, file)
}

// resolveTarget returns the deployment named in args, or the only
// deployment when none is named.
func resolveTarget(store *state.Store, args []string) (state.Deployment, error) {
	if len(args) == 1 {
		return store.Get(args[0])
	}
	deployments, err := store.List()
	if err != nil {
		return state.Deployment{}, err
	}
	switch len(deployments) {
	case 0:
		return state.Deployment{}, state.ErrNotFound
	case 1:
		return deployments[0], nil
	default:
		return state.Deployment{}, errors.New("multiple deployments exist; specify one")
	}
}

func deploymentProcess(deployment state.Deployment) supervisor.Process {
	return supervisor.Process{
		PID:        deployment.PID,
		Command:    deployment.Command,
		StartedAt:  deployment.StartedAt,
		StartToken: deployment.StartToken,
	}
}

// recoverInterruptedDeployments runs only while the caller owns the global
// lifecycle lock. Therefore a transient phase cannot belong to another live
// IOLD command. STARTING intents are adopted when one exact process candidate
// exists, so a later destroy can still terminate the orphan safely.
func recoverInterruptedDeployments(store *state.Store) error {
	deployments, err := store.List()
	if err != nil {
		return err
	}
	for _, deployment := range deployments {
		switch deployment.Phase {
		case state.PhaseDestroyed, state.PhaseDestroying, state.PhaseFailed, state.PhaseCrashed:
			continue
		}

		if deployment.Phase == state.PhaseStarting && deployment.PID == 0 && deployment.Command != "" {
			matches, findErr := supervisor.FindByCommand(deployment.Command, deployment.UpdatedAt)
			if findErr != nil {
				return fmt.Errorf("recover deployment %s: %w", deployment.ID, findErr)
			}
			if len(matches) == 1 {
				match := matches[0]
				if err := store.SetRuntime(deployment.ID, match.PID, deployment.Port, match.Command,
					match.StartedAt, match.StartToken); err != nil {
					return err
				}
				deployment.PID = match.PID
				deployment.Command = match.Command
				deployment.StartedAt = match.StartedAt
				deployment.StartToken = match.StartToken
			}
		}

		if deployment.PID == 0 {
			if err := store.Transition(deployment.ID, deployment.Phase, state.PhaseCrashed,
				"lifecycle operation was interrupted before a runtime identity was recorded"); err != nil {
				return err
			}
			continue
		}

		switch supervisor.Reconcile(deploymentProcess(deployment)) {
		case supervisor.StatusRunning:
			if deployment.Phase == state.PhaseRequested || deployment.Phase == state.PhaseValidating ||
				deployment.Phase == state.PhaseDownloading || deployment.Phase == state.PhaseStarting {
				if err := store.Transition(deployment.ID, deployment.Phase, state.PhaseCrashed,
					"lifecycle operation was interrupted; runtime adopted for cleanup"); err != nil {
					return err
				}
			}
		case supervisor.StatusStale:
			if err := store.Transition(deployment.ID, deployment.Phase, state.PhaseCrashed,
				"runtime process is no longer running"); err != nil {
				return err
			}
		case supervisor.StatusReused:
			if err := store.Transition(deployment.ID, deployment.Phase, state.PhaseCrashed,
				"runtime PID identity no longer matches the recorded process"); err != nil {
				return err
			}
		case supervisor.StatusUnknown:
			return fmt.Errorf("reconcile deployment %s: runtime identity for pid %d is unavailable",
				deployment.ID, deployment.PID)
		}
	}
	return nil
}

func runDestroy(args []string, stdout io.Writer) error {
	all := false
	purge := false
	var id string
	for _, arg := range args {
		switch {
		case arg == "--all":
			all = true
		case arg == "--purge":
			purge = true
		case id == "" && !strings.HasPrefix(arg, "-"):
			id = arg
		default:
			return fmt.Errorf("%w: iold destroy <deployment>|--all [--purge]", ErrUsage)
		}
	}
	if all == (id != "") {
		return fmt.Errorf("%w: iold destroy <deployment>|--all [--purge]", ErrUsage)
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

	var targets []state.Deployment
	if all {
		if targets, err = store.List(); err != nil {
			return err
		}
	} else {
		deployment, err := store.Get(id)
		if err != nil {
			return err
		}
		targets = []state.Deployment{deployment}
	}

	var firstErr error
	for _, deployment := range targets {
		if err := destroyOne(store, deployment, purge, stdout); err != nil {
			fmt.Fprintf(stdout, "destroy %s: %v\n", deployment.ID, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	fmt.Fprintln(stdout, "Note: the RunPod Pod itself is still running and billing continues until you stop it in the RunPod console.")
	return firstErr
}

// destroyOne implements docs/ARCHITECTURE.md §9 for the components that exist
// today: mark DESTROYING, stop the owned runtime process, delete owned
// logs and runtime data, mark DESTROYED, and keep a tombstone row unless
// --purge. Gateway unregistration and tunnels are added by M4.
func destroyOne(store *state.Store, deployment state.Deployment, purge bool, stdout io.Writer) error {
	switch deployment.Phase {
	case state.PhaseDestroyed:
		// Already destroyed: only honour a purge request.
	case state.PhaseDestroying:
		// Interrupted destroy: resume from here.
	case state.PhaseReady, state.PhaseFailed, state.PhaseCrashed, state.PhaseUnregisteredHealthy:
		if err := store.Transition(deployment.ID, deployment.Phase, state.PhaseDestroying, ""); err != nil {
			return err
		}
		deployment.Phase = state.PhaseDestroying
	default:
		// Mid-flight phases route through FAILED, the only legal edge.
		if err := store.Transition(deployment.ID, deployment.Phase, state.PhaseFailed, "destroy requested"); err != nil {
			return err
		}
		if err := store.Transition(deployment.ID, state.PhaseFailed, state.PhaseDestroying, ""); err != nil {
			return err
		}
		deployment.Phase = state.PhaseDestroying
	}

	if deployment.Phase == state.PhaseDestroying && deployment.PID > 0 {
		proc := deploymentProcess(deployment)
		switch supervisor.Reconcile(proc) {
		case supervisor.StatusRunning:
			if err := supervisor.Stop(proc, 10*time.Second); err != nil {
				return fmt.Errorf("stop runtime: %w", err)
			}
			fmt.Fprintf(stdout, "%s: runtime process %d stopped\n", deployment.ID, deployment.PID)
		case supervisor.StatusReused:
			return fmt.Errorf("stop runtime: %w: pid %d identity no longer matches", supervisor.ErrNotOwned, deployment.PID)
		case supervisor.StatusStale:
			// Stop also checks for a surviving process group whose leader is
			// gone; it must not be silently marked destroyed.
			if err := supervisor.Stop(proc, 10*time.Second); err != nil {
				return fmt.Errorf("stop runtime: %w", err)
			}
		case supervisor.StatusUnknown:
			return fmt.Errorf("stop runtime: %w: identity for pid %d is unavailable",
				supervisor.ErrNotOwned, deployment.PID)
		}
	}

	if deployment.Phase == state.PhaseDestroying {
		if err := removeOwnedData(deployment); err != nil {
			return err
		}
		if err := store.Transition(deployment.ID, state.PhaseDestroying, state.PhaseDestroyed, ""); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s: destroyed\n", deployment.ID)
	}

	if purge {
		if err := store.Delete(deployment.ID); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s: state record purged\n", deployment.ID)
	}
	return nil
}

// validIDPattern bounds deployment IDs to a single safe path segment so a
// corrupt or hostile state record can never direct deletion outside the
// IOLD-owned directories (docs/TESTING.md: path-boundary validation).
var validIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

func validateID(id string) error {
	if !validIDPattern.MatchString(id) || strings.Contains(id, "..") {
		return fmt.Errorf("invalid deployment id %q", id)
	}
	return nil
}

// ownedPaths returns the filesystem paths a deployment owns, verifying
// each stays inside the IOLD state directory.
func ownedPaths(id string) ([]string, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	base := stateDir()
	logFile, err := safeChildPath(base, "logs", id+".log")
	if err != nil {
		return nil, err
	}
	deploymentDir, err := safeChildPath(base, "deployments", id)
	if err != nil {
		return nil, err
	}
	return []string{logFile, deploymentDir}, nil
}

func removeOwnedData(deployment state.Deployment) error {
	paths, err := ownedPaths(deployment.ID)
	if err != nil {
		return err
	}
	cachePath := deployment.ModelCacheDir
	if cachePath == "" {
		cachePath, err = deploymentModelCacheDir(deployment.ID)
		if err != nil {
			return err
		}
	}
	owned, ownershipErr := modelCacheOwnedBy(cachePath, deployment)
	if ownershipErr != nil {
		return ownershipErr
	}
	if owned {
		paths = append(paths, cachePath)
	}
	for _, path := range paths {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	return nil
}

func modelCacheOwnedBy(path string, deployment state.Deployment) (bool, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) || filepath.Base(clean) != deployment.ID || deployment.IdempotencyKey == "" {
		return false, fmt.Errorf("unsafe model cache path %q for deployment %s", path, deployment.ID)
	}
	if _, err := os.Stat(clean); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	marker, err := os.ReadFile(filepath.Join(clean, modelCacheOwnerFile))
	if err != nil {
		return false, fmt.Errorf("refusing to remove unowned model cache %s: %w", clean, err)
	}
	if strings.TrimSpace(string(marker)) != deployment.IdempotencyKey {
		return false, fmt.Errorf("refusing to remove model cache %s with mismatched ownership marker", clean)
	}
	return true, nil
}

func logPath(id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	return safeChildPath(stateDir(), "logs", id+".log")
}

func safeChildPath(base string, elements ...string) (string, error) {
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	path := filepath.Join(append([]string{baseAbs}, elements...)...)
	relative, err := filepath.Rel(baseAbs, path)
	if err != nil {
		return "", err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes owned root %q", path, baseAbs)
	}
	return path, nil
}

// stateDir follows docs/ARCHITECTURE.md §5: $IOLD_STATE_DIR, defaulting to
// /workspace/.iold on RunPod (persistent volume) and the user's home
// directory elsewhere.
func stateDir() string {
	if dir := os.Getenv("IOLD_STATE_DIR"); dir != "" {
		return dir
	}
	if info, err := os.Stat("/workspace"); err == nil && info.IsDir() {
		return "/workspace/.iold"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".iold"
	}
	return filepath.Join(home, ".iold")
}

func stateDBPath() string {
	return filepath.Join(stateDir(), "iold.db")
}

func operationLockPath() string {
	return filepath.Join(stateDir(), "operation.lock")
}

func modelCacheDir() string {
	if dir := os.Getenv("IOLD_MODEL_CACHE"); dir != "" {
		return dir
	}
	return filepath.Join(stateDir(), "models")
}

func deploymentModelCacheDir(id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	return safeChildPath(modelCacheDir(), id)
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `IOLD — Instant Open-source LLM Deployment

Usage:
  iold deploy <catalog-alias|hf-model> [--replace]
  iold add <catalog-model>
  iold status [deployment] [--json]
  iold logs [deployment]
  iold destroy [deployment|--all]
  iold plan <hf-model> [--context N] [--vram GiB]
  iold doctor
  iold models
  iold version`)
}
