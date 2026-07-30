// Package supervisor owns the lifecycle of deployment runtime processes
// (docs/ARCHITECTURE.md §5 runtime manager): start detached with captured
// logs, verify PID ownership before acting, stop gracefully then
// forcefully, and reconcile recorded state against the live system.
package supervisor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	ErrNotOwned  = errors.New("process is not owned by this deployment")
	ErrStartFail = errors.New("process failed to start")
)

// Spec describes a runtime process to launch.
type Spec struct {
	Command string
	Args    []string
	Env     []string // appended to the parent environment
	Dir     string
	LogPath string
}

// Process is the persisted identity of a supervised process. Command is
// recorded at start time and compared against the live command line
// before any signal is sent, so a recycled PID is never targeted.
type Process struct {
	PID        int
	Command    string
	LogPath    string
	StartedAt  time.Time
	StartToken string
}

// Start launches the process in its own session so it keeps running
// after the CLI exits (D-007: no reboot persistence, but CLI-independent
// runtime). stdout and stderr are appended to LogPath with 0600.
func Start(spec Spec) (Process, error) {
	if err := os.MkdirAll(filepath.Dir(spec.LogPath), 0o700); err != nil {
		return Process{}, fmt.Errorf("create log dir: %w", err)
	}
	if err := os.Chmod(filepath.Dir(spec.LogPath), 0o700); err != nil {
		return Process{}, fmt.Errorf("secure log dir: %w", err)
	}
	logPipe, err := startLogProxy(spec.LogPath)
	if err != nil {
		return Process{}, err
	}

	cmd := exec.Command(spec.Command, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = append(os.Environ(), spec.Env...)
	cmd.Stdout = logPipe
	cmd.Stderr = logPipe
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		logPipe.Close()
		return Process{}, fmt.Errorf("%w: %v", ErrStartFail, err)
	}
	// Only the runtime retains the pipe's write end. The detached log proxy
	// exits on EOF after the complete runtime process tree closes it.
	logPipe.Close()
	// Reap the child if it exits while this (CLI) process is still
	// alive; otherwise a zombie would keep the PID looking alive.
	go func() { _ = cmd.Wait() }()

	process := Process{
		PID:     cmd.Process.Pid,
		Command: commandString(spec.Command, spec.Args),
		LogPath: spec.LogPath,
	}
	// Capture the post-exec command line and the OS-native start identity.
	// If a very short-lived child exits first, Start still returns its PID so
	// callers can classify the launch as stale rather than losing the event.
	if current, err := cmdline(process.PID); err == nil {
		process.Command = normalize(current)
	}
	if startedAt, token, err := processStartInfo(process.PID); err == nil {
		process.StartedAt = startedAt.UTC()
		process.StartToken = token
	}
	return process, nil
}

func commandString(command string, args []string) string {
	return strings.Join(append([]string{command}, args...), " ")
}

// Alive reports whether a process with this PID currently exists.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Owned reports whether the live process at p.PID is still the process
// recorded at start time. PID, OS-native start identity, wall-clock start
// time, and the exact normalized command must all agree.
func Owned(p Process) bool {
	owned, err := verifyOwned(p)
	return err == nil && owned
}

func verifyOwned(p Process) (bool, error) {
	if !Alive(p.PID) {
		return false, nil
	}
	if p.StartToken == "" || p.StartedAt.IsZero() || p.Command == "" {
		return false, errors.New("recorded process identity is incomplete")
	}
	startedAt, token, err := processStartInfo(p.PID)
	if err != nil {
		return false, err
	}
	if token != p.StartToken || !sameStartTime(startedAt, p.StartedAt) {
		return false, nil
	}
	current, err := cmdline(p.PID)
	if err != nil {
		return false, err
	}
	return normalize(current) == normalize(p.Command), nil
}

func sameStartTime(a, b time.Time) bool {
	delta := a.Sub(b)
	if delta < 0 {
		delta = -delta
	}
	// macOS ps exposes second precision; Linux /proc conversion can differ by
	// one scheduler tick. The native token above remains an exact comparison.
	return delta <= time.Second
}

func normalize(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func cmdline(pid int) (string, error) {
	// Prefer /proc (Linux, the deployment target); fall back to ps for
	// development on macOS.
	if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		return strings.ReplaceAll(strings.TrimRight(string(raw), "\x00"), "\x00", " "), nil
	}
	out, err := exec.Command("ps", "-p", fmt.Sprint(pid), "-o", "command=").Output()
	if err != nil {
		return "", fmt.Errorf("read command line of pid %d: %w", pid, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Status classifies recorded process state against the live system.
type Status string

const (
	StatusRunning Status = "RUNNING" // alive and identity matches
	StatusStale   Status = "STALE"   // recorded PID no longer exists
	StatusReused  Status = "REUSED"  // PID exists but belongs to another process
	StatusUnknown Status = "UNKNOWN" // identity could not be read or is incomplete
)

// Reconcile checks one recorded process (docs/ARCHITECTURE.md §6: on startup
// IOLD reconciles recorded PIDs, ports, and processes).
func Reconcile(p Process) Status {
	if !Alive(p.PID) {
		return StatusStale
	}
	owned, err := verifyOwned(p)
	if err != nil {
		return StatusUnknown
	}
	if !owned {
		return StatusReused
	}
	return StatusRunning
}

// FindByCommand discovers processes matching a persisted STARTING intent.
// Exact command equality and a start-time lower bound keep recovery from
// adopting an older, unrelated process. Callers must still persist and use
// the returned start identity before sending any signal.
func FindByCommand(command string, notBefore time.Time) ([]Process, error) {
	out, err := exec.Command("ps", "-axww", "-o", "pid=", "-o", "command=").Output()
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	want := normalize(command)
	var matches []Process
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid == os.Getpid() {
			continue
		}
		current := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
		if !matchesCommandIntent(normalize(current), want) {
			continue
		}
		startedAt, token, err := processStartInfo(pid)
		if err != nil || startedAt.Before(notBefore.Add(-2*time.Second)) {
			continue
		}
		matches = append(matches, Process{
			PID:        pid,
			Command:    normalize(current),
			StartedAt:  startedAt.UTC(),
			StartToken: token,
		})
	}
	return matches, nil
}

func matchesCommandIntent(current, intent string) bool {
	if current == intent {
		return true
	}
	// Script entry points commonly appear in ps as
	// "python /absolute/path/vllm <args>" even though exec was requested as
	// "vllm <args>". Recovery may match the complete argv suffix, but Stop
	// subsequently persists and requires the exact observed command.
	_, args, found := strings.Cut(intent, " ")
	return found && args != "" && strings.HasSuffix(current, " "+args)
}

// Stop terminates an owned process: SIGTERM to its session, then
// SIGKILL after the grace period. It refuses to signal a PID whose
// identity no longer matches (wrong-target protection). Stopping an
// already-gone process succeeds.
func Stop(p Process, grace time.Duration) error {
	if !Alive(p.PID) {
		if processGroupAlive(p.PID) {
			return fmt.Errorf("%w: process-group %d remains but its verified leader is gone", ErrNotOwned, p.PID)
		}
		return nil
	}
	owned, verifyErr := verifyOwned(p)
	if verifyErr != nil {
		return fmt.Errorf("%w: cannot verify pid %d: %v", ErrNotOwned, p.PID, verifyErr)
	}
	if !owned {
		return fmt.Errorf("%w: pid %d is now %q", ErrNotOwned, p.PID, ownedDetail(p.PID))
	}
	pgid, err := syscall.Getpgid(p.PID)
	if err != nil || pgid != p.PID {
		return fmt.Errorf("%w: pid %d has process group %d", ErrNotOwned, p.PID, pgid)
	}
	// Start put the child in its own session, so its process group ID is
	// its PID; signal and observe the group, not just the leader.
	if err := signalGroup(p.PID, syscall.SIGTERM); err != nil {
		return err
	}

	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !processGroupAlive(p.PID) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := signalGroup(p.PID, syscall.SIGKILL); err != nil {
		return err
	}
	for range 100 {
		if !processGroupAlive(p.PID) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("process group %d did not empty after SIGKILL", p.PID)
}

func signalGroup(pgid int, signal syscall.Signal) error {
	err := syscall.Kill(-pgid, signal)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return fmt.Errorf("signal process group %d with %s: %w", pgid, signal, err)
}

func processGroupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func ownedDetail(pid int) string {
	current, err := cmdline(pid)
	if err != nil {
		return "unreadable"
	}
	return current
}
