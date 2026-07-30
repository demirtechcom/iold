package catalog

import (
	"errors"
	"fmt"
)

var ErrUnknownModel = errors.New("unknown model")

type Model struct {
	Alias              string
	BaseModel          string
	Artifact           string
	ArtifactRevision   string
	Quantization       string
	Runtime            string
	MinimumVRAMGiB     int
	RecommendedVRAMGiB int
	GPUArchitecture    string
	DefaultPort        int
	MaxModelLen        int
}

var models = map[string]Model{
	"qwen3.6-35b-a3b": {
		Alias:              "qwen3.6-35b-a3b",
		BaseModel:          "Qwen/Qwen3.6-35B-A3B",
		Artifact:           "unsloth/Qwen3.6-35B-A3B-NVFP4-Fast",
		ArtifactRevision:   "MOCK_REVISION_PIN_PENDING_GPU_VALIDATION",
		Quantization:       "nvfp4",
		Runtime:            "vllm",
		MinimumVRAMGiB:     32,
		RecommendedVRAMGiB: 48,
		GPUArchitecture:    "blackwell",
		DefaultPort:        8000,
		MaxModelLen:        4096,
	},
}

func Get(alias string) (Model, error) {
	model, ok := models[alias]
	if !ok {
		return Model{}, fmt.Errorf("%w: %s", ErrUnknownModel, alias)
	}
	return model, nil
}

func List() []Model {
	result := make([]Model, 0, len(models))
	for _, model := range models {
		result = append(result, model)
	}
	return result
}
