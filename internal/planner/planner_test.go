package planner

import (
	"errors"
	"testing"

	"github.com/demirtechcom/iold/internal/hf"
)

func llamaLikeConfig() hf.Config {
	return hf.Config{
		Architectures:         []string{"LlamaForCausalLM"},
		ModelType:             "llama",
		NumHiddenLayers:       32,
		NumAttentionHeads:     32,
		NumKeyValueHeads:      8,
		HiddenSize:            4096,
		MaxPositionEmbeddings: 8192,
	}
}

func TestEstimateWeightsFromDtypeCensus(t *testing.T) {
	model := hf.Model{
		ID: "org/8b-bf16",
		Safetensors: &hf.Safetensors{
			Parameters: map[string]int64{"BF16": 8_000_000_000},
			Total:      8_000_000_000,
		},
	}
	est, err := EstimateModel(model, llamaLikeConfig(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	if est.WeightBytes != 16_000_000_000 {
		t.Errorf("weights = %d, want 16e9", est.WeightBytes)
	}
	// 2 * 32 layers * 8 kv heads * 128 head_dim * 2 bytes * 4096 tokens
	if want := int64(2 * 32 * 8 * 128 * 2 * 4096); est.KVCacheBytes != want {
		t.Errorf("kv = %d, want %d", est.KVCacheBytes, want)
	}
	if !est.KnownArchitecture {
		t.Error("llama should be a known architecture")
	}
	if est.Quantization != "" {
		t.Errorf("quantization = %q, want none", est.Quantization)
	}
}

func TestEstimateMixedDtypesAndUnknownDtype(t *testing.T) {
	model := hf.Model{
		ID: "org/quant",
		Safetensors: &hf.Safetensors{
			Parameters: map[string]int64{"U4": 1000, "F32": 100, "MYSTERY": 10},
		},
	}
	est, err := EstimateModel(model, hf.Config{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(1000/2 + 100*4 + 10*2); est.WeightBytes != want {
		t.Errorf("weights = %d, want %d", est.WeightBytes, want)
	}
	if !est.KVCacheUnknown {
		t.Error("empty config should mark kv cache unknown")
	}
}

// Regression: the hub reports packed AWQ/GPTQ weights as I32 with the
// logical parameter count (Qwen/Qwen2.5-72B-Instruct-AWQ: 70.5B "I32"
// params in a ~41 GiB artifact). Integer dtypes must cost the quant
// scheme's bit width, not 4 bytes.
func TestEstimatePackedAWQUsesQuantBits(t *testing.T) {
	model := hf.Model{
		ID: "Qwen/Qwen2.5-72B-Instruct-AWQ",
		Safetensors: &hf.Safetensors{
			Parameters: map[string]int64{"I32": 70_464_307_200, "F16": 2_493_554_688},
		},
	}
	cfg := hf.Config{QuantizationConfig: &struct {
		QuantMethod string `json:"quant_method"`
		Bits        int    `json:"bits"`
	}{QuantMethod: "awq", Bits: 4}}
	est, err := EstimateModel(model, cfg, 4096)
	if err != nil {
		t.Fatal(err)
	}
	weightGiB := GiB(est.WeightBytes)
	if weightGiB < 35 || weightGiB > 45 {
		t.Errorf("weights = %.1f GiB, want ~40 GiB", weightGiB)
	}
}

func TestEstimateRejectsMissingSizeData(t *testing.T) {
	_, err := EstimateModel(hf.Model{ID: "org/no-data"}, hf.Config{}, 4096)
	if !errors.Is(err, ErrNoSizeData) {
		t.Fatalf("err = %v, want ErrNoSizeData", err)
	}
}

func TestContextClampedToMaxPositionEmbeddings(t *testing.T) {
	model := hf.Model{
		ID:          "org/m",
		Safetensors: &hf.Safetensors{Parameters: map[string]int64{"BF16": 1}},
	}
	est, err := EstimateModel(model, llamaLikeConfig(), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if est.ContextTokens != 8192 {
		t.Errorf("context = %d, want clamp to 8192", est.ContextTokens)
	}
}

func TestDetectQuantization(t *testing.T) {
	cases := []struct {
		name  string
		model hf.Model
		cfg   hf.Config
		want  string
	}{
		{"from config", hf.Model{ID: "x"}, hf.Config{QuantizationConfig: &struct {
			QuantMethod string `json:"quant_method"`
			Bits        int    `json:"bits"`
		}{QuantMethod: "awq", Bits: 4}}, "awq"},
		{"from id", hf.Model{ID: "org/Model-NVFP4-Fast"}, hf.Config{}, "nvfp4"},
		{"from tag", hf.Model{ID: "org/m", Tags: []string{"gptq"}}, hf.Config{}, "gptq"},
		{"none", hf.Model{ID: "org/plain"}, hf.Config{}, ""},
	}
	for _, tc := range cases {
		if got := DetectQuantization(tc.model, tc.cfg); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestAssessVerdicts(t *testing.T) {
	// 24 GiB GPU: usable = 24 * 0.9 = 21.6 GiB; comfort edge = 18.36 GiB.
	cases := []struct {
		totalGiB float64
		want     Verdict
	}{
		{10, VerdictFits},
		{20, VerdictTight},
		{30, VerdictTooBig},
	}
	for _, tc := range cases {
		est := Estimate{TotalBytes: int64(tc.totalGiB * (1 << 30))}
		if got := Assess(est, 24*1024); got != tc.want {
			t.Errorf("%.0f GiB on 24 GiB: got %s, want %s", tc.totalGiB, got, tc.want)
		}
	}
}
