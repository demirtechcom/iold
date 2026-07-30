package cli

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/demirtechcom/iold/internal/doctor"
	"github.com/demirtechcom/iold/internal/hf"
)

const testRevision = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// fakeHub serves the minimal hub API surface plan uses: model info,
// config.json, and search.
func fakeHub(t *testing.T, handler http.HandlerFunc) *hf.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := hf.NewClient("")
	client.BaseURL = server.URL
	return client
}

func planProbes(vramMiB int) fakeProbes {
	return fakeProbes{
		gpus: []doctor.GPU{{
			Name:            "NVIDIA GeForce RTX 4090",
			DriverVersion:   "575.51.03",
			VRAMMiB:         vramMiB,
			ComputeCapMajor: 12,
		}},
		diskFree: 500 << 30,
	}
}

func TestPlanFitsOnDetectedGPU(t *testing.T) {
	client := fakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/org/small-7b":
			w.Write([]byte(`{"id":"org/small-7b","sha":"` + testRevision + `","gated":false,"safetensors":{"parameters":{"BF16":7000000000},"total":7000000000}}`))
		case "/org/small-7b/raw/" + testRevision + "/config.json":
			w.Write([]byte(`{"architectures":["LlamaForCausalLM"],"model_type":"llama","num_hidden_layers":32,"num_attention_heads":32,"num_key_value_heads":8,"hidden_size":4096,"max_position_embeddings":8192}`))
		default:
			http.NotFound(w, r)
		}
	})
	var stdout bytes.Buffer
	if err := runPlan([]string{"org/small-7b"}, planProbes(24_576), client, &stdout); err != nil {
		t.Fatalf("runPlan: %v\n%s", err, stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Verdict: FITS") {
		t.Errorf("expected FITS verdict:\n%s", out)
	}
	if !strings.Contains(out, "RTX 4090") {
		t.Errorf("expected detected GPU in output:\n%s", out)
	}
}

func TestPlanTooBigSuggestsQuantizedVariant(t *testing.T) {
	client := fakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/org/big-70b":
			w.Write([]byte(`{"id":"org/big-70b","sha":"` + testRevision + `","gated":false,"safetensors":{"parameters":{"BF16":70000000000},"total":70000000000}}`))
		case r.URL.Path == "/org/big-70b/raw/"+testRevision+"/config.json",
			r.URL.Path == "/quant/big-70b-AWQ/raw/"+testRevision+"/config.json":
			w.Write([]byte(`{"architectures":["LlamaForCausalLM"],"model_type":"llama","num_hidden_layers":80,"num_attention_heads":64,"num_key_value_heads":8,"hidden_size":8192,"max_position_embeddings":8192}`))
		case r.URL.Path == "/api/models" && strings.Contains(r.URL.Query().Get("search"), "AWQ"):
			w.Write([]byte(`[{"id":"quant/big-70b-AWQ","downloads":9000}]`))
		case r.URL.Path == "/api/models":
			w.Write([]byte(`[]`))
		case r.URL.Path == "/api/models/quant/big-70b-AWQ":
			w.Write([]byte(`{"id":"quant/big-70b-AWQ","sha":"` + testRevision + `","gated":false,"downloads":9000,"tags":["awq"],"safetensors":{"parameters":{"U4":70000000000},"total":70000000000}}`))
		default:
			http.NotFound(w, r)
		}
	})
	var stdout bytes.Buffer
	// 96 GiB GPU: 70B bf16 (~140 GiB) cannot fit, the 4-bit AWQ (~35 GiB) can.
	if err := runPlan([]string{"org/big-70b", "--vram", "96"}, planProbes(0), client, &stdout); err != nil {
		t.Fatalf("runPlan: %v\n%s", err, stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Verdict: TOO_BIG") {
		t.Errorf("expected TOO_BIG verdict:\n%s", out)
	}
	if !strings.Contains(out, "quant/big-70b-AWQ") {
		t.Errorf("expected AWQ suggestion:\n%s", out)
	}
}

func TestPlanGatedWithoutTokenFailsWithGuidance(t *testing.T) {
	client := fakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"meta/gated","gated":"manual"}`))
	})
	var stdout bytes.Buffer
	err := runPlan([]string{"meta/gated"}, planProbes(24_576), client, &stdout)
	if !errors.Is(err, hf.ErrAuthRequired) {
		t.Fatalf("err = %v, want ErrAuthRequired", err)
	}
	if ExitCode(err) != ExitEnvironment {
		t.Errorf("exit code = %d, want %d", ExitCode(err), ExitEnvironment)
	}
}

func TestPlanUnknownModelExitsNotFound(t *testing.T) {
	client := fakeHub(t, http.NotFound)
	var stdout bytes.Buffer
	err := runPlan([]string{"no/such-model"}, planProbes(24_576), client, &stdout)
	if !errors.Is(err, hf.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if ExitCode(err) != ExitNotFound {
		t.Errorf("exit code = %d, want %d", ExitCode(err), ExitNotFound)
	}
}

func TestPlanUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"--context"},
		{"m", "--vram", "zero"},
		{"m", "extra"},
	} {
		if err := runPlan(args, planProbes(1024), hf.NewClient(""), &bytes.Buffer{}); !errors.Is(err, ErrUsage) {
			t.Errorf("args %v: err = %v, want ErrUsage", args, err)
		}
	}
}
