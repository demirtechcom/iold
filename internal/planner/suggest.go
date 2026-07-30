package planner

import (
	"context"
	"sort"
	"strings"

	"github.com/demirtechcom/iold/internal/hf"
)

// Suggestion is a quantized variant of a model that the planner
// predicts will fit the host GPU.
type Suggestion struct {
	Estimate  Estimate
	Verdict   Verdict
	Downloads int64
	Gated     bool
}

// searched quantization families, in query order. GGUF is excluded:
// the vLLM-first scope does not serve GGUF artifacts.
var variantQueries = []string{"AWQ", "GPTQ", "FP8", "NVFP4", "4bit"}

const (
	searchLimitPerQuery = 8
	maxCandidateFetches = 12
	maxSuggestions      = 5
)

// servableByVLLM rejects artifact formats vLLM does not serve
// (docs/ARCHITECTURE.md: GGUF and MLX are excluded from the vLLM-first
// scope).
func servableByVLLM(m hf.Model) bool {
	id := strings.ToLower(m.ID)
	if strings.Contains(id, "mlx") || strings.Contains(id, "gguf") {
		return false
	}
	for _, tag := range m.Tags {
		switch strings.ToLower(tag) {
		case "mlx", "gguf":
			return false
		}
	}
	return true
}

// SuggestQuantized searches Hugging Face for quantized variants of
// baseID and returns the ones predicted to fit vramMiB, most
// downloaded first. Individual candidate failures are skipped, not
// fatal: a suggestion list is best-effort by nature.
func SuggestQuantized(ctx context.Context, client *hf.Client, baseID string, vramMiB, contextTokens int) ([]Suggestion, error) {
	shortName := baseID
	if _, after, found := strings.Cut(baseID, "/"); found {
		shortName = after
	}

	seen := map[string]bool{strings.ToLower(baseID): true}
	var candidates []hf.Model
	for _, quant := range variantQueries {
		results, err := client.Search(ctx, shortName+" "+quant, searchLimitPerQuery)
		if err != nil {
			return nil, err
		}
		for _, m := range results {
			key := strings.ToLower(m.ID)
			// Require the base model's name in the variant ID so a
			// broad search cannot suggest an unrelated model.
			if seen[key] || !strings.Contains(key, strings.ToLower(shortName)) {
				continue
			}
			if !servableByVLLM(m) {
				continue
			}
			seen[key] = true
			candidates = append(candidates, m)
		}
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Downloads > candidates[j].Downloads })
	if len(candidates) > maxCandidateFetches {
		candidates = candidates[:maxCandidateFetches]
	}

	var suggestions []Suggestion
	for _, candidate := range candidates {
		model, err := client.GetModel(ctx, candidate.ID)
		if err != nil || model.SHA == "" {
			continue
		}
		cfg, err := client.GetConfigAtRevision(ctx, candidate.ID, model.SHA)
		if err != nil {
			cfg = hf.Config{}
		}
		est, err := EstimateModel(model, cfg, contextTokens)
		if err != nil {
			continue
		}
		if DetectQuantization(model, cfg) == "" {
			continue
		}
		verdict := Assess(est, vramMiB)
		if verdict == VerdictTooBig {
			continue
		}
		suggestions = append(suggestions, Suggestion{
			Estimate:  est,
			Verdict:   verdict,
			Downloads: model.Downloads,
			Gated:     model.Gated.IsGated,
		})
		if len(suggestions) == maxSuggestions {
			break
		}
	}
	return suggestions, nil
}
