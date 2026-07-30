// Package doctor validates that the host can run IOLD deployments.
// All system access goes through the Probes interface so checks are
// fully testable without a GPU (docs/TASKS.md M2-04).
package doctor

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var ErrChecksFailed = errors.New("doctor checks failed")

type Status string

const (
	StatusPass Status = "PASS"
	StatusWarn Status = "WARN"
	StatusFail Status = "FAIL"
)

// GPU is the subset of nvidia-smi output the checks need.
type GPU struct {
	Name            string
	DriverVersion   string
	VRAMMiB         int
	ComputeCapMajor int
	ComputeCapMinor int
}

// Probes abstracts every system interaction doctor performs.
type Probes interface {
	GPUs() ([]GPU, error)
	DiskFreeBytes(path string) (uint64, error)
	CheckEndpoint(url string) error
}

// Requirements describes what the target deployment environment must provide.
type Requirements struct {
	GPUArchitecture string
	ExpectedGPUName string
	MinComputeMajor int
	MinDriverMajor  int
	MinVRAMGiB      int
	MinDiskFreeGiB  int
	CacheDir        string
	Endpoints       []string
}

// Blackwell floor: RTX PRO 6000 Blackwell is SM 12.0 and requires the
// R570+ driver series (CUDA 12.8). Disk floor covers one NVFP4 35B
// artifact plus headroom for a transactional replace of a second model.
func DefaultRequirements(cacheDir string) Requirements {
	return Requirements{
		GPUArchitecture: "blackwell",
		ExpectedGPUName: "RTX PRO 6000",
		MinComputeMajor: 10,
		MinDriverMajor:  570,
		MinVRAMGiB:      32,
		MinDiskFreeGiB:  100,
		CacheDir:        cacheDir,
		Endpoints:       []string{"https://huggingface.co"},
	}
}

type Check struct {
	Name   string
	Status Status
	Detail string
}

type Report struct {
	Checks []Check
}

func (r Report) OK() bool {
	for _, c := range r.Checks {
		if c.Status == StatusFail {
			return false
		}
	}
	return true
}

// Run executes every check and always returns a full report; it never
// stops at the first failure so the user sees all problems at once.
func Run(p Probes, req Requirements) Report {
	var report Report
	report.Checks = append(report.Checks, gpuChecks(p, req)...)
	report.Checks = append(report.Checks, diskCheck(p, req))
	for _, endpoint := range req.Endpoints {
		report.Checks = append(report.Checks, networkCheck(p, endpoint))
	}
	return report
}

func gpuChecks(p Probes, req Requirements) []Check {
	gpus, err := p.GPUs()
	if err != nil || len(gpus) == 0 {
		detail := "no NVIDIA GPU detected"
		if err != nil {
			detail = err.Error()
		}
		return []Check{{
			Name:   "nvidia-driver",
			Status: StatusFail,
			Detail: detail + "; use the supported IOLD RunPod template instead of a generic image",
		}}
	}

	gpu := gpus[0]
	checks := []Check{driverCheck(gpu, req)}

	if strings.Contains(gpu.Name, req.ExpectedGPUName) {
		checks = append(checks, Check{"gpu-model", StatusPass, gpu.Name})
	} else {
		checks = append(checks, Check{
			"gpu-model", StatusWarn,
			fmt.Sprintf("expected %s, found %s; deployment is only validated on %s",
				req.ExpectedGPUName, gpu.Name, req.ExpectedGPUName),
		})
	}

	if gpu.ComputeCapMajor >= req.MinComputeMajor {
		checks = append(checks, Check{
			"gpu-architecture", StatusPass,
			fmt.Sprintf("%s (compute capability %d.%d)", req.GPUArchitecture, gpu.ComputeCapMajor, gpu.ComputeCapMinor),
		})
	} else {
		checks = append(checks, Check{
			"gpu-architecture", StatusFail,
			fmt.Sprintf("compute capability %d.%d is below %d.0; NVFP4 requires a %s GPU",
				gpu.ComputeCapMajor, gpu.ComputeCapMinor, req.MinComputeMajor, req.GPUArchitecture),
		})
	}

	requiredMiB := req.MinVRAMGiB * 1024
	if gpu.VRAMMiB >= requiredMiB {
		checks = append(checks, Check{
			"vram", StatusPass,
			fmt.Sprintf("%d MiB available, %d MiB required", gpu.VRAMMiB, requiredMiB),
		})
	} else {
		checks = append(checks, Check{
			"vram", StatusFail,
			fmt.Sprintf("%d MiB available, %d MiB required", gpu.VRAMMiB, requiredMiB),
		})
	}
	return checks
}

func driverCheck(gpu GPU, req Requirements) Check {
	major, err := driverMajor(gpu.DriverVersion)
	if err != nil {
		return Check{"nvidia-driver", StatusFail, fmt.Sprintf("cannot parse driver version %q", gpu.DriverVersion)}
	}
	if major < req.MinDriverMajor {
		return Check{
			"nvidia-driver", StatusFail,
			fmt.Sprintf("driver %s is below the R%d floor required for %s",
				gpu.DriverVersion, req.MinDriverMajor, req.GPUArchitecture),
		}
	}
	return Check{"nvidia-driver", StatusPass, "driver " + gpu.DriverVersion}
}

func driverMajor(version string) (int, error) {
	head, _, _ := strings.Cut(version, ".")
	return strconv.Atoi(strings.TrimSpace(head))
}

func diskCheck(p Probes, req Requirements) Check {
	free, err := p.DiskFreeBytes(req.CacheDir)
	if err != nil {
		return Check{"disk", StatusFail, fmt.Sprintf("cannot stat model cache %s: %v", req.CacheDir, err)}
	}
	freeGiB := free / (1 << 30)
	detail := fmt.Sprintf("%d GiB free at %s, %d GiB required", freeGiB, req.CacheDir, req.MinDiskFreeGiB)
	if freeGiB < uint64(req.MinDiskFreeGiB) {
		return Check{"disk", StatusFail, detail}
	}
	return Check{"disk", StatusPass, detail}
}

func networkCheck(p Probes, endpoint string) Check {
	if err := p.CheckEndpoint(endpoint); err != nil {
		return Check{"network", StatusFail, fmt.Sprintf("%s unreachable: %v", endpoint, err)}
	}
	return Check{"network", StatusPass, endpoint + " reachable"}
}
