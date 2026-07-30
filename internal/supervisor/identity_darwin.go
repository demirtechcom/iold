//go:build darwin

package supervisor

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const psStartLayout = "Mon Jan _2 15:04:05 2006"

func processStartInfo(pid int) (time.Time, string, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return time.Time{}, "", fmt.Errorf("read start time of pid %d: %w", pid, err)
	}
	startedAt, err := time.ParseInLocation(psStartLayout, strings.TrimSpace(string(out)), time.Local)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("parse start time of pid %d: %w", pid, err)
	}
	startedAt = startedAt.UTC()
	return startedAt, "darwin:" + strconv.FormatInt(startedAt.Unix(), 10), nil
}
