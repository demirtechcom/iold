package catalog

import (
	"errors"
	"testing"
)

func TestGetFirstSupportedModel(t *testing.T) {
	model, err := Get("qwen3.6-35b-a3b")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if model.Artifact != "unsloth/Qwen3.6-35B-A3B-NVFP4-Fast" {
		t.Fatalf("unexpected artifact: %q", model.Artifact)
	}
}

func TestGetRejectsUnknownModel(t *testing.T) {
	_, err := Get("arbitrary/model")
	if !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("expected ErrUnknownModel, got %v", err)
	}
}
