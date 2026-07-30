// Package planner decides whether a Hugging Face model fits the host
// GPU before anything is downloaded or started (D-017). Estimates are
// heuristic and deliberately conservative: a Fits verdict is a
// prediction, not a guarantee, and readiness checks remain mandatory.
package planner

import (
	"errors"
	"fmt"
	"strings"

	"github.com/demirtechcom/iold/internal/hf"
)

var ErrNoSizeData = errors.New("model publishes no safetensors parameter data; cannot estimate size")

const (
	// vLLM's default gpu_memory_utilization: the fraction of VRAM it
	// will actually claim.
	vllmMemoryUtilization = 0.90
	// CUDA context, allocator slack, and vLLM runtime overhead.
	fixedOverheadBytes = 2 << 30
	// A Fits verdict requires headroom below the usable budget; between
	// this fraction and 100% of budget the verdict is Tight.
	comfortFraction = 0.85

	DefaultContextTokens = 4096
)

type Verdict string

const (
	VerdictFits   Verdict = "FITS"
	VerdictTight  Verdict = "TIGHT"
	VerdictTooBig Verdict = "TOO_BIG"
)

type Estimate struct {
	ModelID       string
	ParamCount    int64
	Quantization  string // "" when unquantized
	ContextTokens int
	WeightBytes   int64
	KVCacheBytes  int64 // 0 when config lacks attention geometry
	OverheadBytes int64
	TotalBytes    int64
	Architecture  string
	// KnownArchitecture is false when the model type is not on the
	// vLLM-supported list; deployment may still work but is unproven.
	KnownArchitecture bool
	// KVCacheUnknown marks estimates where config.json did not expose
	// layer/head geometry, so TotalBytes understates real need.
	KVCacheUnknown bool
}

// bytes per parameter for each safetensors dtype reported by the hub.
var dtypeBytes = map[string]float64{
	"F64": 8, "I64": 8, "U64": 8,
	"F32": 4, "I32": 4, "U32": 4,
	"BF16": 2, "F16": 2, "I16": 2, "U16": 2,
	"F8_E4M3": 1, "F8_E5M2": 1, "I8": 1, "U8": 1, "BOOL": 1,
	"F4": 0.5, "NF4": 0.5, "I4": 0.5, "U4": 0.5,
}

// model_type values vLLM is known to serve. Unknown types produce a
// warning, not a rejection: the list ages faster than vLLM does.
var knownModelTypes = map[string]bool{
	"llama": true, "llama4": true, "mistral": true, "mixtral": true,
	"qwen2": true, "qwen2_moe": true, "qwen3": true, "qwen3_moe": true,
	"gemma": true, "gemma2": true, "gemma3": true, "gemma3_text": true,
	"phi": true, "phi3": true, "phimoe": true,
	"deepseek_v2": true, "deepseek_v3": true,
	"falcon": true, "gpt2": true, "gpt_neox": true, "gpt_bigcode": true,
	"starcoder2": true, "olmo": true, "olmo2": true, "granite": true,
	"internlm2": true, "baichuan": true, "chatglm": true, "glm4": true,
	"stablelm": true, "mpt": true, "opt": true, "bloom": true,
	"cohere": true, "command-r": true, "exaone": true, "minicpm": true,
	"nemotron": true, "smollm3": true,
}

var quantMarkers = []string{"nvfp4", "awq", "gptq", "fp8", "int8", "int4", "4bit", "4-bit", "8bit", "8-bit", "bnb", "mxfp4"}

// DetectQuantization reports the quantization scheme from config,
// tags, or the repo name, or "" when the model looks unquantized.
func DetectQuantization(model hf.Model, cfg hf.Config) string {
	if cfg.QuantizationConfig != nil && cfg.QuantizationConfig.QuantMethod != "" {
		return strings.ToLower(cfg.QuantizationConfig.QuantMethod)
	}
	haystacks := append([]string{strings.ToLower(model.ID)}, model.Tags...)
	for _, h := range haystacks {
		h = strings.ToLower(h)
		for _, marker := range quantMarkers {
			if strings.Contains(h, marker) {
				return strings.TrimRight(strings.ReplaceAll(marker, "-", ""), "")
			}
		}
	}
	return ""
}

// EstimateModel predicts VRAM need for serving the model with vLLM at
// the given context length.
func EstimateModel(model hf.Model, cfg hf.Config, contextTokens int) (Estimate, error) {
	if contextTokens <= 0 {
		contextTokens = DefaultContextTokens
	}
	if cfg.MaxPositionEmbeddings > 0 && contextTokens > cfg.MaxPositionEmbeddings {
		contextTokens = cfg.MaxPositionEmbeddings
	}

	est := Estimate{
		ModelID:       model.ID,
		Quantization:  DetectQuantization(model, cfg),
		ContextTokens: contextTokens,
		OverheadBytes: fixedOverheadBytes,
	}

	if model.Safetensors == nil || len(model.Safetensors.Parameters) == 0 {
		return Estimate{}, fmt.Errorf("%w: %s", ErrNoSizeData, model.ID)
	}
	// AWQ/GPTQ-style schemes pack sub-byte weights into integer
	// tensors, and the hub census reports the logical parameter count
	// under the storage dtype (e.g. 70B "I32" for a 4-bit 72B model).
	// For quantized models, integer dtypes therefore cost the scheme's
	// bit width, not the storage width.
	quantBytes := quantBytesPerParam(est.Quantization, cfg)
	var weight float64
	for dtype, count := range model.Safetensors.Parameters {
		est.ParamCount += count
		dtype = strings.ToUpper(dtype)
		bytes, ok := dtypeBytes[dtype]
		if !ok {
			bytes = 2 // unknown dtype: assume 16-bit
		}
		if quantBytes > 0 && (strings.HasPrefix(dtype, "I") || strings.HasPrefix(dtype, "U")) {
			bytes = quantBytes
		}
		weight += float64(count) * bytes
	}
	est.WeightBytes = int64(weight)

	if len(cfg.Architectures) > 0 {
		est.Architecture = cfg.Architectures[0]
	}
	est.KnownArchitecture = knownModelTypes[strings.ToLower(cfg.ModelType)]

	est.KVCacheBytes = kvCacheBytes(cfg, contextTokens)
	est.KVCacheUnknown = est.KVCacheBytes == 0

	est.TotalBytes = est.WeightBytes + est.KVCacheBytes + est.OverheadBytes
	return est, nil
}

// quantBytesPerParam returns the effective bytes per quantized weight,
// or 0 when the model is unquantized or the scheme's width is unknown.
func quantBytesPerParam(quant string, cfg hf.Config) float64 {
	if quant == "" {
		return 0
	}
	if cfg.QuantizationConfig != nil && cfg.QuantizationConfig.Bits > 0 {
		return float64(cfg.QuantizationConfig.Bits) / 8
	}
	switch quant {
	case "awq", "gptq", "nvfp4", "int4", "4bit", "nf4", "bnb", "bitsandbytes", "mxfp4":
		return 0.5
	case "fp8", "int8", "8bit":
		return 1
	default:
		return 0
	}
}

// kvCacheBytes sizes one full-context KV cache in fp16:
// 2 (K and V) x layers x kv_heads x head_dim x 2 bytes x tokens.
func kvCacheBytes(cfg hf.Config, contextTokens int) int64 {
	heads := cfg.NumKeyValueHeads
	if heads == 0 {
		heads = cfg.NumAttentionHeads
	}
	headDim := cfg.HeadDim
	if headDim == 0 && cfg.NumAttentionHeads > 0 {
		headDim = cfg.HiddenSize / cfg.NumAttentionHeads
	}
	if cfg.NumHiddenLayers == 0 || heads == 0 || headDim == 0 {
		return 0
	}
	return int64(2) * int64(cfg.NumHiddenLayers) * int64(heads) *
		int64(headDim) * 2 * int64(contextTokens)
}

// Assess compares an estimate against the GPU's usable VRAM budget.
func Assess(est Estimate, vramMiB int) Verdict {
	usable := float64(vramMiB) * (1 << 20) * vllmMemoryUtilization
	total := float64(est.TotalBytes)
	switch {
	case total <= usable*comfortFraction:
		return VerdictFits
	case total <= usable:
		return VerdictTight
	default:
		return VerdictTooBig
	}
}

func GiB(bytes int64) float64 {
	return float64(bytes) / (1 << 30)
}
