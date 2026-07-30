package doctor

import (
	"errors"
	"strings"
	"testing"
)

type fakeProbes struct {
	gpus     []GPU
	gpuErr   error
	diskFree uint64
	diskErr  error
	netErr   error
}

func (f fakeProbes) GPUs() ([]GPU, error)                 { return f.gpus, f.gpuErr }
func (f fakeProbes) DiskFreeBytes(string) (uint64, error) { return f.diskFree, f.diskErr }
func (f fakeProbes) CheckEndpoint(string) error           { return f.netErr }

func rtxPro6000() GPU {
	return GPU{
		Name:            "NVIDIA RTX PRO 6000 Blackwell Workstation Edition",
		DriverVersion:   "575.51.03",
		VRAMMiB:         97887,
		ComputeCapMajor: 12,
		ComputeCapMinor: 0,
	}
}

func healthy() fakeProbes {
	return fakeProbes{
		gpus:     []GPU{rtxPro6000()},
		diskFree: 500 << 30,
	}
}

func find(t *testing.T, r Report, name string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q missing from report: %+v", name, r.Checks)
	return Check{}
}

func TestHealthyBlackwellPassesAllChecks(t *testing.T) {
	report := Run(healthy(), DefaultRequirements("/workspace/models"))
	if !report.OK() {
		t.Fatalf("expected OK report, got %+v", report.Checks)
	}
	for _, name := range []string{"nvidia-driver", "gpu-model", "gpu-architecture", "vram", "disk", "network"} {
		if c := find(t, report, name); c.Status != StatusPass {
			t.Fatalf("check %s = %s (%s), want PASS", name, c.Status, c.Detail)
		}
	}
}

func TestMissingNvidiaSMIFailsWithTemplateGuidance(t *testing.T) {
	probes := fakeProbes{gpuErr: errors.New("nvidia-smi failed: executable not found"), diskFree: 500 << 30}
	report := Run(probes, DefaultRequirements("/workspace/models"))
	if report.OK() {
		t.Fatal("expected failing report")
	}
	c := find(t, report, "nvidia-driver")
	if c.Status != StatusFail {
		t.Fatalf("nvidia-driver = %s, want FAIL", c.Status)
	}
	if want := "supported IOLD RunPod template"; !strings.Contains(c.Detail, want) {
		t.Fatalf("detail %q missing guidance %q", c.Detail, want)
	}
}

func TestPreBlackwellArchitectureFails(t *testing.T) {
	gpu := rtxPro6000()
	gpu.Name = "NVIDIA GeForce RTX 4090"
	gpu.ComputeCapMajor, gpu.ComputeCapMinor = 8, 9
	gpu.VRAMMiB = 24564
	probes := healthy()
	probes.gpus = []GPU{gpu}

	report := Run(probes, DefaultRequirements("/workspace/models"))
	if report.OK() {
		t.Fatal("expected failing report")
	}
	if c := find(t, report, "gpu-architecture"); c.Status != StatusFail {
		t.Fatalf("gpu-architecture = %s, want FAIL", c.Status)
	}
	if c := find(t, report, "gpu-model"); c.Status != StatusWarn {
		t.Fatalf("gpu-model = %s, want WARN", c.Status)
	}
	if c := find(t, report, "vram"); c.Status != StatusFail {
		t.Fatalf("vram = %s, want FAIL for 24 GiB card", c.Status)
	}
}

func TestOldDriverFails(t *testing.T) {
	probes := healthy()
	probes.gpus[0].DriverVersion = "550.90.07"
	report := Run(probes, DefaultRequirements("/workspace/models"))
	if c := find(t, report, "nvidia-driver"); c.Status != StatusFail {
		t.Fatalf("nvidia-driver = %s, want FAIL for R550", c.Status)
	}
}

func TestLowDiskFails(t *testing.T) {
	probes := healthy()
	probes.diskFree = 20 << 30
	report := Run(probes, DefaultRequirements("/workspace/models"))
	if c := find(t, report, "disk"); c.Status != StatusFail {
		t.Fatalf("disk = %s, want FAIL for 20 GiB free", c.Status)
	}
}

func TestUnreachableEndpointFails(t *testing.T) {
	probes := healthy()
	probes.netErr = errors.New("dial tcp: i/o timeout")
	report := Run(probes, DefaultRequirements("/workspace/models"))
	if c := find(t, report, "network"); c.Status != StatusFail {
		t.Fatalf("network = %s, want FAIL", c.Status)
	}
}

func TestParseNvidiaSMI(t *testing.T) {
	out := "NVIDIA RTX PRO 6000 Blackwell Workstation Edition, 575.51.03, 97887, 12.0\n"
	gpus, err := parseNvidiaSMI(out)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(gpus) != 1 {
		t.Fatalf("got %d GPUs, want 1", len(gpus))
	}
	want := rtxPro6000()
	if gpus[0] != want {
		t.Fatalf("got %+v, want %+v", gpus[0], want)
	}
}

func TestParseNvidiaSMIRejectsMalformedLine(t *testing.T) {
	if _, err := parseNvidiaSMI("garbage line without commas\n"); err == nil {
		t.Fatal("expected error for malformed line")
	}
}
