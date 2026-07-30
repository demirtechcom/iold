package hf

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newFakeHub(t *testing.T, routes map[string]string) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	client := NewClient("")
	client.BaseURL = server.URL
	return client, server
}

func TestGetModelParsesGatedBoolAndString(t *testing.T) {
	client, _ := newFakeHub(t, map[string]string{
		"/api/models/open/model":  `{"id":"open/model","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","gated":false,"safetensors":{"parameters":{"BF16":1000},"total":1000}}`,
		"/api/models/gated/model": `{"id":"gated/model","gated":"manual"}`,
	})

	open, err := client.GetModel(context.Background(), "open/model")
	if err != nil {
		t.Fatal(err)
	}
	if open.Gated.IsGated {
		t.Error("open model reported gated")
	}
	if open.Safetensors.Total != 1000 {
		t.Errorf("total = %d, want 1000", open.Safetensors.Total)
	}
	if open.SHA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("sha = %q", open.SHA)
	}

	gated, err := client.GetModel(context.Background(), "gated/model")
	if err != nil {
		t.Fatal(err)
	}
	if !gated.Gated.IsGated || gated.Gated.Mode != "manual" {
		t.Errorf("gated = %+v, want manual gate", gated.Gated)
	}
}

func TestGetConfigAtImmutableRevision(t *testing.T) {
	const revision = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	client, _ := newFakeHub(t, map[string]string{
		"/org/model/raw/" + revision + "/config.json": `{"model_type":"llama","num_hidden_layers":32}`,
	})
	cfg, err := client.GetConfigAtRevision(context.Background(), "org/model", revision)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelType != "llama" || cfg.NumHiddenLayers != 32 {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestGetModelNotFound(t *testing.T) {
	client, _ := newFakeHub(t, nil)
	_, err := client.GetModel(context.Background(), "no/such")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestAuthErrorsMapToErrAuthRequired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	client := NewClient("")
	client.BaseURL = server.URL
	_, err := client.GetModel(context.Background(), "gated/model")
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("err = %v, want ErrAuthRequired", err)
	}
}

func TestTokenSentAsBearer(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"id":"m"}`))
	}))
	defer server.Close()
	client := NewClient("hf_secret")
	client.BaseURL = server.URL
	if _, err := client.GetModel(context.Background(), "m"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer hf_secret" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

func TestConfigResolvedFlattensTextConfig(t *testing.T) {
	client, _ := newFakeHub(t, map[string]string{
		"/mm/model/raw/main/config.json": `{"architectures":["MultiModal"],"model_type":"mm","text_config":{"model_type":"llama","num_hidden_layers":32,"num_attention_heads":32,"hidden_size":4096}}`,
	})
	cfg, err := client.GetConfig(context.Background(), "mm/model")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NumHiddenLayers != 32 || cfg.ModelType != "llama" {
		t.Errorf("resolved config = %+v", cfg)
	}
	if len(cfg.Architectures) == 0 || cfg.Architectures[0] != "MultiModal" {
		t.Errorf("architectures not inherited: %+v", cfg.Architectures)
	}
}

func TestSearchQueryEncoding(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("search")
		w.Write([]byte(`[{"id":"a"},{"id":"b"}]`))
	}))
	defer server.Close()
	client := NewClient("")
	client.BaseURL = server.URL
	models, err := client.Search(context.Background(), "Llama 3 AWQ", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || gotQuery != "Llama 3 AWQ" {
		t.Errorf("models = %d, query = %q", len(models), gotQuery)
	}
}
