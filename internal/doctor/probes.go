package doctor

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SystemProbes is the production implementation of Probes.
type SystemProbes struct {
	HTTPTimeout time.Duration
}

func NewSystemProbes() *SystemProbes {
	return &SystemProbes{HTTPTimeout: 10 * time.Second}
}

func (s *SystemProbes) GPUs() ([]GPU, error) {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=name,driver_version,memory.total,compute_cap",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi failed: %w", err)
	}
	return parseNvidiaSMI(string(out))
}

func parseNvidiaSMI(out string) ([]GPU, error) {
	var gpus []GPU
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) != 4 {
			return nil, fmt.Errorf("unexpected nvidia-smi line: %q", line)
		}
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		vram, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("bad memory.total %q: %w", fields[2], err)
		}
		majorStr, minorStr, _ := strings.Cut(fields[3], ".")
		major, err := strconv.Atoi(majorStr)
		if err != nil {
			return nil, fmt.Errorf("bad compute_cap %q: %w", fields[3], err)
		}
		minor, _ := strconv.Atoi(minorStr)
		gpus = append(gpus, GPU{
			Name:            fields[0],
			DriverVersion:   fields[1],
			VRAMMiB:         vram,
			ComputeCapMajor: major,
			ComputeCapMinor: minor,
		})
	}
	return gpus, nil
}

// DiskFreeBytes reports free space for the filesystem that will hold the
// model cache, walking up to the nearest existing ancestor so the check
// works before the cache directory has been created.
func (s *SystemProbes) DiskFreeBytes(path string) (uint64, error) {
	dir := path
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return 0, fmt.Errorf("no existing ancestor for %s", path)
		}
		dir = parent
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}

func (s *SystemProbes) CheckEndpoint(url string) error {
	client := &http.Client{Timeout: s.HTTPTimeout}
	resp, err := client.Head(url)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
