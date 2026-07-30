// Package preflight owns the single host-validation path used by every model
// source before IOLD creates or replaces a deployment.
package preflight

import (
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/demirtechcom/iold/internal/doctor"
	"github.com/demirtechcom/iold/internal/supervisor"
)

type Spec struct {
	Probes          doctor.Probes
	RuntimeBinary   string
	CacheDir        string
	Endpoint        string
	MinDriverMajor  int
	MinComputeMajor int
	MinVRAMMiB      int
	MinDiskGiB      int
	PreferredPort   int
	ReservedPorts   []int
	// Ports owned by deployments that this operation will replace are
	// considered available after their verified shutdown succeeds.
	ReleasablePorts []int

	LookPath func(string) (string, error)
}

type Result struct {
	Port int
}

func Run(spec Spec) (Result, error) {
	if spec.Probes == nil {
		return Result{}, fmt.Errorf("%w: preflight probes are not configured", doctor.ErrChecksFailed)
	}
	var failures []string
	gpus, err := spec.Probes.GPUs()
	if err != nil || len(gpus) == 0 {
		failures = append(failures, fmt.Sprintf("NVIDIA GPU detection failed: %v", err))
	} else {
		// vLLM is launched without tensor parallelism or a GPU selector, so
		// validate the primary GPU it will actually choose.
		gpu := gpus[0]
		if spec.MinDriverMajor > 0 {
			major, parseErr := driverMajor(gpu.DriverVersion)
			if parseErr != nil {
				failures = append(failures, fmt.Sprintf("cannot parse NVIDIA driver %q", gpu.DriverVersion))
			} else if major < spec.MinDriverMajor {
				failures = append(failures, fmt.Sprintf("NVIDIA driver %s is below required R%d", gpu.DriverVersion, spec.MinDriverMajor))
			}
		}
		if gpu.ComputeCapMajor < spec.MinComputeMajor {
			failures = append(failures, fmt.Sprintf("compute capability %d.%d is below required %d.0",
				gpu.ComputeCapMajor, gpu.ComputeCapMinor, spec.MinComputeMajor))
		}
		if gpu.VRAMMiB < spec.MinVRAMMiB {
			failures = append(failures, fmt.Sprintf("GPU has %d MiB VRAM; %d MiB required", gpu.VRAMMiB, spec.MinVRAMMiB))
		}
	}

	if spec.MinDiskGiB > 0 {
		free, diskErr := spec.Probes.DiskFreeBytes(spec.CacheDir)
		if diskErr != nil {
			failures = append(failures, fmt.Sprintf("cannot inspect model cache %s: %v", spec.CacheDir, diskErr))
		} else if free/(1<<30) < uint64(spec.MinDiskGiB) {
			failures = append(failures, fmt.Sprintf("model cache %s has %d GiB free; %d GiB required",
				spec.CacheDir, free/(1<<30), spec.MinDiskGiB))
		}
	}
	if spec.Endpoint != "" {
		if err := spec.Probes.CheckEndpoint(spec.Endpoint); err != nil {
			failures = append(failures, fmt.Sprintf("endpoint %s is unreachable: %v", spec.Endpoint, err))
		}
	}
	lookPath := spec.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if spec.RuntimeBinary == "" {
		failures = append(failures, "runtime binary is empty")
	} else if _, err := lookPath(spec.RuntimeBinary); err != nil {
		failures = append(failures, fmt.Sprintf("runtime %q is unavailable: %v", spec.RuntimeBinary, err))
	}

	port := 0
	if spec.PreferredPort <= 0 || spec.PreferredPort > 65535 {
		failures = append(failures, fmt.Sprintf("invalid preferred port %d", spec.PreferredPort))
	} else if slices.Contains(spec.ReleasablePorts, spec.PreferredPort) &&
		!slices.Contains(spec.ReservedPorts, spec.PreferredPort) {
		port = spec.PreferredPort
	} else {
		port, err = supervisor.AllocatePort(spec.PreferredPort, spec.ReservedPorts)
		if err != nil {
			failures = append(failures, err.Error())
		}
	}

	if len(failures) > 0 {
		return Result{}, fmt.Errorf("%w: %s", doctor.ErrChecksFailed, strings.Join(failures, "; "))
	}
	return Result{Port: port}, nil
}

func driverMajor(version string) (int, error) {
	head, _, _ := strings.Cut(version, ".")
	return strconv.Atoi(strings.TrimSpace(head))
}
