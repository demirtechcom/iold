package planner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/demirtechcom/iold/internal/hf"
)

const suggestionRevision = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestSuggestFiltersUnrelatedAndUnservableModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models":
			// Same result set for every quant query: one good AWQ
			// variant, one MLX build, one unrelated model.
			json.NewEncoder(w).Encode([]hf.Model{
				{ID: "quant/base-7b-AWQ", Downloads: 500},
				{ID: "mlx-community/base-7b-4bit", Downloads: 9999},
				{ID: "other/completely-different-AWQ", Downloads: 8888},
			})
		case r.URL.Path == "/api/models/quant/base-7b-AWQ":
			w.Write([]byte(`{"id":"quant/base-7b-AWQ","sha":"` + suggestionRevision + `","gated":false,"downloads":500,"tags":["awq"],"safetensors":{"parameters":{"U4":7000000000},"total":7000000000}}`))
		case strings.HasSuffix(r.URL.Path, "/raw/"+suggestionRevision+"/config.json"):
			w.Write([]byte(`{"model_type":"llama","num_hidden_layers":32,"num_attention_heads":32,"num_key_value_heads":8,"hidden_size":4096}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := hf.NewClient("")
	client.BaseURL = server.URL

	suggestions, err := SuggestQuantized(context.Background(), client, "org/base-7b", 24*1024, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(suggestions) != 1 {
		t.Fatalf("suggestions = %d, want exactly the AWQ variant: %+v", len(suggestions), suggestions)
	}
	if suggestions[0].Estimate.ModelID != "quant/base-7b-AWQ" {
		t.Errorf("suggested %s", suggestions[0].Estimate.ModelID)
	}
}
