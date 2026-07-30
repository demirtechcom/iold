package preflight

import (
	"errors"
	"strings"
	"testing"

	"github.com/demirtechcom/iold/internal/doctor"
)

type fakeProbes struct {
	gpus     []doctor.GPU
	diskFree uint64
	network  error
}

func (f fakeProbes) GPUs() ([]doctor.GPU, error)          { return f.gpus, nil }
func (f fakeProbes) DiskFreeBytes(string) (uint64, error) { return f.diskFree, nil }
func (f fakeProbes) CheckEndpoint(string) error           { return f.network }

func passingSpec() Spec {
	return Spec{
		Probes: fakeProbes{
			gpus: []doctor.GPU{{
				DriverVersion: "575.51.03", VRAMMiB: 96 * 1024, ComputeCapMajor: 12,
			}},
			diskFree: 500 << 30,
		},
		RuntimeBinary:   "vllm",
		CacheDir:        "/workspace/.iold/models/dep",
		Endpoint:        "https://huggingface.co",
		MinDriverMajor:  570,
		MinComputeMajor: 10,
		MinVRAMMiB:      32 * 1024,
		MinDiskGiB:      100,
		PreferredPort:   18000,
		ReleasablePorts: []int{18000},
		LookPath: func(string) (string, error) {
			return "/usr/local/bin/vllm", nil
		},
	}
}

func TestRunChecksFullHostAndReturnsPort(t *testing.T) {
	result, err := Run(passingSpec())
	if err != nil {
		t.Fatal(err)
	}
	if result.Port != 18000 {
		t.Fatalf("port = %d, want 18000", result.Port)
	}
}

func TestRunAggregatesHardwareDiskNetworkAndRuntimeFailures(t *testing.T) {
	spec := passingSpec()
	spec.Probes = fakeProbes{
		gpus:     []doctor.GPU{{DriverVersion: "525.1", VRAMMiB: 8 * 1024, ComputeCapMajor: 8}},
		diskFree: 1 << 30,
		network:  errors.New("offline"),
	}
	spec.LookPath = func(string) (string, error) { return "", errors.New("missing") }
	_, err := Run(spec)
	if !errors.Is(err, doctor.ErrChecksFailed) {
		t.Fatalf("err = %v, want ErrChecksFailed", err)
	}
	for _, fragment := range []string{"below required R570", "compute capability", "VRAM", "model cache", "unreachable", "runtime"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error missing %q: %v", fragment, err)
		}
	}
}
