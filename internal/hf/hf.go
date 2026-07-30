// Package hf resolves Hugging Face model metadata for the deployment
// planner (D-017). It reads the public hub API and, when HF_TOKEN is
// set, authenticates so gated models resolve too (D-018). The token is
// never logged and never appears in returned errors.
package hf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

var (
	ErrNotFound     = errors.New("model not found on Hugging Face")
	ErrAuthRequired = errors.New("model is gated or private; set HF_TOKEN to a token with access")
	ErrNoRevision   = errors.New("response from Hugging Face did not include an immutable revision SHA")
)

const DefaultBaseURL = "https://huggingface.co"

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func NewClient(token string) *Client {
	return &Client{
		BaseURL: DefaultBaseURL,
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Gated models report `"gated": "auto"` or `"manual"`; open models
// report `false`. Normalize to a bool plus the raw mode.
type Gated struct {
	IsGated bool
	Mode    string
}

func (g *Gated) UnmarshalJSON(data []byte) error {
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		g.IsGated = b
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("unexpected gated value %s", data)
	}
	g.IsGated = true
	g.Mode = s
	return nil
}

// MarshalJSON mirrors the hub's wire format: a mode string when
// gated, false otherwise.
func (g Gated) MarshalJSON() ([]byte, error) {
	if !g.IsGated {
		return []byte("false"), nil
	}
	if g.Mode == "" {
		return []byte("true"), nil
	}
	return json.Marshal(g.Mode)
}

// Safetensors holds the hub's parameter census: dtype name -> count.
type Safetensors struct {
	Parameters map[string]int64 `json:"parameters"`
	Total      int64            `json:"total"`
}

type Model struct {
	ID          string       `json:"id"`
	SHA         string       `json:"sha"`
	Gated       Gated        `json:"gated"`
	Private     bool         `json:"private"`
	Downloads   int64        `json:"downloads"`
	Tags        []string     `json:"tags"`
	PipelineTag string       `json:"pipeline_tag"`
	Safetensors *Safetensors `json:"safetensors"`
}

// Config is the subset of a model's config.json the planner needs.
// Multimodal repos nest the language model under text_config.
type Config struct {
	Architectures         []string `json:"architectures"`
	ModelType             string   `json:"model_type"`
	NumHiddenLayers       int      `json:"num_hidden_layers"`
	NumAttentionHeads     int      `json:"num_attention_heads"`
	NumKeyValueHeads      int      `json:"num_key_value_heads"`
	HiddenSize            int      `json:"hidden_size"`
	HeadDim               int      `json:"head_dim"`
	MaxPositionEmbeddings int      `json:"max_position_embeddings"`
	QuantizationConfig    *struct {
		QuantMethod string `json:"quant_method"`
		Bits        int    `json:"bits"`
	} `json:"quantization_config"`
	TextConfig *Config `json:"text_config"`
}

// Resolved flattens a nested text_config so callers always read the
// language-model dimensions from the top level.
func (c Config) Resolved() Config {
	if c.TextConfig != nil && c.NumHiddenLayers == 0 {
		nested := c.TextConfig.Resolved()
		if len(nested.Architectures) == 0 {
			nested.Architectures = c.Architectures
		}
		if nested.ModelType == "" {
			nested.ModelType = c.ModelType
		}
		if nested.QuantizationConfig == nil {
			nested.QuantizationConfig = c.QuantizationConfig
		}
		return nested
	}
	return c
}

// GetModel fetches hub metadata for a model ID such as
// "unsloth/Qwen3.6-35B-A3B-NVFP4-Fast".
func (c *Client) GetModel(ctx context.Context, id string) (Model, error) {
	var model Model
	err := c.getJSON(ctx, "/api/models/"+id, &model)
	if err != nil {
		return Model{}, fmt.Errorf("resolve %s: %w", id, err)
	}
	return model, nil
}

// GetConfig fetches the model's config.json from the main revision.
func (c *Client) GetConfig(ctx context.Context, id string) (Config, error) {
	return c.GetConfigAtRevision(ctx, id, "main")
}

// GetConfigAtRevision fetches the exact config used by the runtime plan. A
// commit SHA prevents main from changing between estimation and download.
func (c *Client) GetConfigAtRevision(ctx context.Context, id, revision string) (Config, error) {
	if revision == "" {
		return Config{}, ErrNoRevision
	}
	var cfg Config
	if err := c.getJSON(ctx, "/"+id+"/raw/"+url.PathEscape(revision)+"/config.json", &cfg); err != nil {
		return Config{}, fmt.Errorf("config for %s: %w", id, err)
	}
	return cfg.Resolved(), nil
}

// Search returns text-generation models matching the query, most
// downloaded first.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]Model, error) {
	path := "/api/models?pipeline_tag=text-generation&sort=downloads&direction=-1" +
		"&search=" + url.QueryEscape(query) + "&limit=" + strconv.Itoa(limit)
	var models []Model
	if err := c.getJSON(ctx, path, &models); err != nil {
		return nil, fmt.Errorf("search %q: %w", query, err)
	}
	return models, nil
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return ErrAuthRequired
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("hugging face returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}
