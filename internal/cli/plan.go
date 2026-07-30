package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/demirtechcom/iold/internal/doctor"
	"github.com/demirtechcom/iold/internal/hf"
	"github.com/demirtechcom/iold/internal/planner"
)

// runPlan implements `iold plan <hf-model> [--context N] [--vram GiB]`:
// resolve any Hugging Face model, estimate whether it fits this host's
// GPU, and suggest quantized variants when it does not (D-017).
func runPlan(args []string, probes doctor.Probes, client *hf.Client, stdout io.Writer) error {
	usage := fmt.Errorf("%w: iold plan <hf-model> [--context N] [--vram GiB]", ErrUsage)

	var modelID string
	contextTokens := planner.DefaultContextTokens
	vramGiB := 0
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--context" || args[i] == "--vram":
			if i+1 >= len(args) {
				return usage
			}
			value, err := strconv.Atoi(args[i+1])
			if err != nil || value <= 0 {
				return usage
			}
			if args[i] == "--context" {
				contextTokens = value
			} else {
				vramGiB = value
			}
			i++
		case modelID == "" && !strings.HasPrefix(args[i], "-"):
			modelID = args[i]
		default:
			return usage
		}
	}
	if modelID == "" {
		return usage
	}

	vramMiB := vramGiB * 1024
	if vramMiB == 0 {
		gpus, err := probes.GPUs()
		if err != nil || len(gpus) == 0 {
			return fmt.Errorf("no GPU detected (%v); pass --vram <GiB> to plan for a target GPU", err)
		}
		for _, gpu := range gpus {
			if gpu.VRAMMiB > vramMiB {
				vramMiB = gpu.VRAMMiB
			}
		}
		fmt.Fprintf(stdout, "GPU: %s, %d MiB VRAM\n", gpus[0].Name, vramMiB)
	} else {
		fmt.Fprintf(stdout, "GPU: assumed %d GiB VRAM (--vram)\n", vramGiB)
	}

	ctx := context.Background()
	model, err := client.GetModel(ctx, modelID)
	if err != nil {
		return err
	}
	if model.Gated.IsGated && client.Token == "" {
		return fmt.Errorf("%w: %s", hf.ErrAuthRequired, modelID)
	}
	if err := validateRevision(model.SHA); err != nil {
		return fmt.Errorf("resolve immutable revision for %s: %w", modelID, err)
	}
	cfg, err := client.GetConfigAtRevision(ctx, modelID, model.SHA)
	if err != nil && !errors.Is(err, hf.ErrNotFound) {
		return err
	}

	est, err := planner.EstimateModel(model, cfg, contextTokens)
	if err != nil {
		return err
	}
	verdict := planner.Assess(est, vramMiB)
	printEstimate(stdout, est, verdict)

	if verdict != planner.VerdictTooBig {
		return nil
	}
	fmt.Fprintln(stdout, "\nSearching for quantized variants that fit...")
	suggestions, err := planner.SuggestQuantized(ctx, client, modelID, vramMiB, contextTokens)
	if err != nil {
		return fmt.Errorf("variant search failed: %w", err)
	}
	if len(suggestions) == 0 {
		fmt.Fprintln(stdout, "No fitting quantized variant found. Try a larger GPU or a smaller model.")
		return nil
	}
	for _, s := range suggestions {
		gated := ""
		if s.Gated {
			gated = " (gated: needs HF_TOKEN)"
		}
		fmt.Fprintf(stdout, "  %-7s %s  ~%.1f GiB, %s, %d downloads%s\n",
			s.Verdict, s.Estimate.ModelID, planner.GiB(s.Estimate.TotalBytes),
			quantLabel(s.Estimate.Quantization), s.Downloads, gated)
	}
	return nil
}

func printEstimate(stdout io.Writer, est planner.Estimate, verdict planner.Verdict) {
	fmt.Fprintf(stdout, "Model: %s\n", est.ModelID)
	fmt.Fprintf(stdout, "  parameters:    %.1fB\n", float64(est.ParamCount)/1e9)
	fmt.Fprintf(stdout, "  quantization:  %s\n", quantLabel(est.Quantization))
	fmt.Fprintf(stdout, "  weights:       ~%.1f GiB\n", planner.GiB(est.WeightBytes))
	if est.KVCacheUnknown {
		fmt.Fprintf(stdout, "  kv cache:      unknown (config.json lacks attention geometry); total understates need\n")
	} else {
		fmt.Fprintf(stdout, "  kv cache:      ~%.1f GiB at %d tokens\n", planner.GiB(est.KVCacheBytes), est.ContextTokens)
	}
	fmt.Fprintf(stdout, "  overhead:      ~%.1f GiB\n", planner.GiB(est.OverheadBytes))
	fmt.Fprintf(stdout, "  total:         ~%.1f GiB\n", planner.GiB(est.TotalBytes))
	if est.Architecture != "" && !est.KnownArchitecture {
		fmt.Fprintf(stdout, "  WARN architecture %s is not on the known vLLM support list; it may not serve\n", est.Architecture)
	}
	fmt.Fprintf(stdout, "Verdict: %s\n", verdict)
	if verdict == planner.VerdictFits || verdict == planner.VerdictTight {
		fmt.Fprintln(stdout, "Estimates are conservative heuristics; readiness checks still decide.")
	}
}

func quantLabel(quant string) string {
	if quant == "" {
		return "none"
	}
	return quant
}
