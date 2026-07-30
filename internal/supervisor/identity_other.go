//go:build !linux && !darwin

package supervisor

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func processStartInfo(pid int) (time.Time, string, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return time.Time{}, "", fmt.Errorf("read start time of pid %d: %w", pid, err)
	}
	startedAt, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.TrimSpace(string(out)), time.Local)
	if err != nil {
		return time.Time{}, "", err
	}
	startedAt = startedAt.UTC()
	return startedAt, "unix:" + strconv.FormatInt(startedAt.Unix(), 10), nil
}

// exited reports whether the process is gone or has become a zombie awaiting
// reaping, in which case its live command line will never appear.
func exited(pid int) bool {
	out, err := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return true
	}
	state := strings.TrimSpace(string(out))
	return state == "" || strings.HasPrefix(state, "Z")
}
