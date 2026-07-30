package fakes

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// GatewayOptions selects gateway failure modes. The zero value is a
// healthy gateway that accepts any bearer token.
type GatewayOptions struct {
	Token          string        // when set, requests require Bearer Token
	UnavailableFor int           // first N register calls return 503
	ResponseDelay  time.Duration // delay before every response (client timeout tests)
	FailUnregister bool          // DELETE returns 500
}

type registration struct {
	endpoint       map[string]any
	idempotencyKey string
}

// Gateway implements mocks/gateway-openapi.yaml in-process.
type Gateway struct {
	*httptest.Server
	opts GatewayOptions

	mu            sync.Mutex
	registrations map[string]registration // by deployment_id
	registerCalls int
}

func NewGateway(opts GatewayOptions) *Gateway {
	g := &Gateway{opts: opts, registrations: map[string]registration{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/model-endpoints", g.auth(g.handleRegister))
	mux.HandleFunc("GET /internal/model-endpoints/{deploymentId}", g.auth(g.handleGet))
	mux.HandleFunc("DELETE /internal/model-endpoints/{deploymentId}", g.auth(g.handleUnregister))
	g.Server = httptest.NewServer(mux)
	return g
}

// Registered reports whether a deployment currently has an endpoint record.
func (g *Gateway) Registered(deploymentID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.registrations[deploymentID]
	return ok
}

// RegisterCalls reports how many register requests reached the gateway.
func (g *Gateway) RegisterCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.registerCalls
}

func (g *Gateway) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if g.opts.ResponseDelay > 0 {
			time.Sleep(g.opts.ResponseDelay)
		}
		if g.opts.Token != "" && r.Header.Get("Authorization") != "Bearer "+g.opts.Token {
			http.Error(w, `{"error": "unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (g *Gateway) handleRegister(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.registerCalls++
	if g.registerCalls <= g.opts.UnavailableFor {
		http.Error(w, `{"error": "gateway unavailable"}`, http.StatusServiceUnavailable)
		return
	}

	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		http.Error(w, `{"error": "Idempotency-Key header required"}`, http.StatusBadRequest)
		return
	}
	var req struct {
		DeploymentID string `json:"deployment_id"`
		Model        string `json:"model"`
		BaseURL      string `json:"base_url"`
		APIKey       string `json:"api_key"`
		Runtime      string `json:"runtime"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		req.DeploymentID == "" || req.Model == "" || req.BaseURL == "" || req.APIKey == "" || req.Runtime == "" {
		http.Error(w, `{"error": "missing required fields"}`, http.StatusBadRequest)
		return
	}

	if existing, ok := g.registrations[req.DeploymentID]; ok {
		if existing.idempotencyKey == key {
			// Idempotent replay: return the original response.
			writeJSON(w, http.StatusCreated, existing.endpoint)
			return
		}
		http.Error(w, `{"error": "deployment already registered"}`, http.StatusConflict)
		return
	}

	endpoint := map[string]any{
		"id":            fmt.Sprintf("ep-%s", req.DeploymentID),
		"deployment_id": req.DeploymentID,
		"status":        "registering",
	}
	g.registrations[req.DeploymentID] = registration{endpoint: endpoint, idempotencyKey: key}
	writeJSON(w, http.StatusCreated, endpoint)
}

func (g *Gateway) handleGet(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	reg, ok := g.registrations[r.PathValue("deploymentId")]
	if !ok {
		http.Error(w, `{"error": "not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, reg.endpoint)
}

func (g *Gateway) handleUnregister(w http.ResponseWriter, r *http.Request) {
	if g.opts.FailUnregister {
		http.Error(w, `{"error": "internal error"}`, http.StatusInternalServerError)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	// Per the contract, deleting an absent registration also returns 204.
	delete(g.registrations, r.PathValue("deploymentId"))
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}
