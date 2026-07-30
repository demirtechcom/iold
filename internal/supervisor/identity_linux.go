//go:build linux

package supervisor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	clockTicksOnce sync.Once
	clockTicksHz   int64 = 100 // Linux USER_HZ fallback used by supported amd64 images.
)

// processStartInfo reads field 22 from /proc/<pid>/stat. The native tick
// value is the authoritative identity token; the wall-clock value is derived
// from btime for diagnostics and the persisted StartedAt cross-check.
func processStartInfo(pid int) (time.Time, string, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return time.Time{}, "", err
	}
	line := string(raw)
	closeParen := strings.LastIndex(line, ")")
	if closeParen < 0 {
		return time.Time{}, "", fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	// After comm, fields[0] is field 3 (state), so field 22 is index 19.
	fields := strings.Fields(line[closeParen+1:])
	if len(fields) <= 19 {
		return time.Time{}, "", fmt.Errorf("short /proc/%d/stat", pid)
	}
	startTicks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil || startTicks < 0 {
		return time.Time{}, "", fmt.Errorf("bad process start ticks %q", fields[19])
	}

	bootSeconds, err := linuxBootTime()
	if err != nil {
		return time.Time{}, "", err
	}
	hz := linuxClockTicks()
	seconds := startTicks / hz
	nanos := (startTicks % hz) * int64(time.Second) / hz
	startedAt := time.Unix(bootSeconds+seconds, nanos).UTC()
	return startedAt, "linux:" + fields[19], nil
}

// exited reports whether the process is gone or has become a zombie awaiting
// reaping. A zombie keeps its /proc entry and start identity but will never
// populate a live command line, so identity capture must stop retrying.
func exited(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	line := string(raw)
	closeParen := strings.LastIndex(line, ")")
	if closeParen < 0 {
		return false
	}
	// Field 3 (state) is the first token after the comm field.
	fields := strings.Fields(line[closeParen+1:])
	return len(fields) > 0 && (fields[0] == "Z" || fields[0] == "X" || fields[0] == "x")
}

func linuxBootTime() (int64, error) {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "btime" {
			value, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			return value, nil
		}
	}
	return 0, errors.New("btime missing from /proc/stat")
}

func linuxClockTicks() int64 {
	clockTicksOnce.Do(func() {
		out, err := exec.Command("getconf", "CLK_TCK").Output()
		if err != nil {
			return
		}
		if value, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil && value > 0 {
			clockTicksHz = value
		}
	})
	return clockTicksHz
}
