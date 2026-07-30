// Package fakes provides in-process fake servers for component tests
// (docs/TESTING.md M5-01): a fake vLLM OpenAI-compatible server and a fake
// gateway registration API matching mocks/gateway-openapi.yaml.
package fakes

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
)

// VLLMOptions selects the failure mode a test wants to exercise. The
// zero value is a healthy server.
type VLLMOptions struct {
	Model              string // served model name; defaults to "fake-model"
	APIKey             string // when set, endpoints require Bearer APIKey
	HealthyAfter       int    // /health returns 503 for this many calls first (slow startup)
	FailLoad           bool   // /health always returns 500 (model failed to load)
	MalformedModelList bool   // /v1/models returns truncated JSON
}

type VLLM struct {
	*httptest.Server
	opts        VLLMOptions
	healthCalls atomic.Int64
}

func NewVLLM(opts VLLMOptions) *VLLM {
	if opts.Model == "" {
		opts.Model = "fake-model"
	}
	v := &VLLM{opts: opts}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", v.handleHealth)
	mux.HandleFunc("GET /v1/models", v.auth(v.handleModels))
	mux.HandleFunc("POST /v1/chat/completions", v.auth(v.handleChat))
	v.Server = httptest.NewServer(mux)
	return v
}

// HealthCalls reports how many times /health has been probed.
func (v *VLLM) HealthCalls() int { return int(v.healthCalls.Load()) }

func (v *VLLM) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if v.opts.APIKey != "" && r.Header.Get("Authorization") != "Bearer "+v.opts.APIKey {
			http.Error(w, `{"error": "invalid api key"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (v *VLLM) handleHealth(w http.ResponseWriter, _ *http.Request) {
	calls := v.healthCalls.Add(1)
	switch {
	case v.opts.FailLoad:
		http.Error(w, "engine failed to load model", http.StatusInternalServerError)
	case int(calls) <= v.opts.HealthyAfter:
		http.Error(w, "starting", http.StatusServiceUnavailable)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

func (v *VLLM) handleModels(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if v.opts.MalformedModelList {
		fmt.Fprint(w, `{"object": "list", "data": [{"id": `)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   []map[string]any{{"id": v.opts.Model, "object": "model"}},
	})
}

func (v *VLLM) handleChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error": "bad request"}`, http.StatusBadRequest)
		return
	}
	if req.Model != v.opts.Model {
		http.Error(w, `{"error": "model not found"}`, http.StatusNotFound)
		return
	}
	if req.Stream {
		v.streamChat(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "chat.completion",
		"model":  v.opts.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "pong"},
			"finish_reason": "stop",
		}},
	})
}

func (v *VLLM) streamChat(w http.ResponseWriter) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, token := range []string{"po", "ng"} {
		chunk, _ := json.Marshal(map[string]any{
			"object": "chat.completion.chunk",
			"model":  v.opts.Model,
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{"content": token},
			}},
		})
		fmt.Fprintf(w, "data: %s\n\n", chunk)
		flusher.Flush()
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// ReadSSEContent concatenates the delta content of a text/event-stream
// chat response body, for assertions in streaming tests.
func ReadSSEContent(body string) string {
	var content strings.Builder
	for line := range strings.SplitSeq(body, "\n") {
		data, found := strings.CutPrefix(line, "data: ")
		if !found || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		for _, choice := range chunk.Choices {
			content.WriteString(choice.Delta.Content)
		}
	}
	return content.String()
}
