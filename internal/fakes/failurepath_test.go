package fakes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/demirtechcom/iold/internal/retry"
)

// fastPolicy keeps failure-path tests well under the 5s budget.
func fastPolicy(maxAttempts int) retry.Policy {
	return retry.Policy{
		MaxAttempts: maxAttempts,
		BaseDelay:   time.Millisecond,
		MaxDelay:    5 * time.Millisecond,
		Multiplier:  2,
	}
}

// registerBody is a valid register payload for the fake gateway.
func registerBody(deploymentID string) []byte {
	body, _ := json.Marshal(map[string]string{
		"deployment_id": deploymentID,
		"model":         "fake-model",
		"base_url":      "http://127.0.0.1:8000",
		"api_key":       "sk-test",
		"runtime":       "vllm",
	})
	return body
}

// registerOnce POSTs one register request and classifies the outcome the
// way the real gateway client must: 5xx is retryable, 4xx is permanent.
func registerOnce(ctx context.Context, client *http.Client, url, token, idempotencyKey string, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/internal/model-endpoints", bytes.NewReader(payload))
	if err != nil {
		return retry.Permanent(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err // network/timeout errors are retryable
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	switch {
	case resp.StatusCode < 300:
		return nil
	case resp.StatusCode >= 500:
		return fmt.Errorf("gateway returned %d", resp.StatusCode)
	default:
		return retry.Permanent(fmt.Errorf("gateway returned %d", resp.StatusCode))
	}
}

// keyRecorder proxies requests to a backend while recording every
// Idempotency-Key header it sees, so tests can assert key reuse on the
// wire rather than trusting the client code under test.
type keyRecorder struct {
	*httptest.Server
	mu   sync.Mutex
	keys []string
}

func newKeyRecorder(t *testing.T, backendURL string) *keyRecorder {
	t.Helper()
	rec := &keyRecorder{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.keys = append(rec.keys, r.Header.Get("Idempotency-Key"))
		rec.mu.Unlock()

		body, _ := io.ReadAll(r.Body)
		fwd, err := http.NewRequestWithContext(r.Context(), r.Method, backendURL+r.URL.Path, bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		fwd.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(fwd)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	return rec
}

func (rec *keyRecorder) Keys() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.keys...)
}

// Failure-path matrix: gateway unavailable -> retry registration.
func TestRetryGatewayUnavailableThenRecovers(t *testing.T) {
	gateway := NewGateway(GatewayOptions{UnavailableFor: 2})
	defer gateway.Close()
	recorder := newKeyRecorder(t, gateway.URL)
	defer recorder.Close()

	const key = "idem-key-1"
	payload := registerBody("dep-1")
	attempts := 0
	err := retry.Do(context.Background(), fastPolicy(4), func(ctx context.Context) error {
		attempts++
		return registerOnce(ctx, http.DefaultClient, recorder.URL, "", key, payload)
	})
	if err != nil {
		t.Fatalf("register after recovery failed: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if got := gateway.RegisterCalls(); got != 3 {
		t.Fatalf("RegisterCalls = %d, want 3", got)
	}
	if !gateway.Registered("dep-1") {
		t.Fatal("deployment not registered after retries")
	}
	keys := recorder.Keys()
	if len(keys) != 3 {
		t.Fatalf("recorded %d requests, want 3", len(keys))
	}
	for i, got := range keys {
		if got != key {
			t.Fatalf("attempt %d sent Idempotency-Key %q, want %q reused on every attempt", i+1, got, key)
		}
	}
}

// Failure-path matrix: auth failure is permanent, never retried.
func TestRetryGatewayAuthFailureIsPermanent(t *testing.T) {
	gateway := NewGateway(GatewayOptions{Token: "right-token"})
	defer gateway.Close()

	payload := registerBody("dep-auth")
	attempts := 0
	err := retry.Do(context.Background(), fastPolicy(5), func(ctx context.Context) error {
		attempts++
		return registerOnce(ctx, http.DefaultClient, gateway.URL, "wrong-token", "idem-key-auth", payload)
	})
	if err == nil {
		t.Fatal("expected auth failure, got nil")
	}
	if !retry.IsPermanent(err) {
		t.Fatalf("auth failure not classified permanent: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want exactly 1 for permanent error", attempts)
	}
	if gateway.Registered("dep-auth") {
		t.Fatal("deployment must not be registered on auth failure")
	}
}

// Failure-path matrix: gateway timeout is retryable and retried to exhaustion.
func TestRetryGatewayClientTimeoutIsRetryable(t *testing.T) {
	gateway := NewGateway(GatewayOptions{ResponseDelay: 150 * time.Millisecond})
	defer gateway.Close()

	policy := fastPolicy(3)
	policy.AttemptTimeout = 30 * time.Millisecond
	payload := registerBody("dep-timeout")
	attempts := 0
	err := retry.Do(context.Background(), policy, func(ctx context.Context) error {
		attempts++
		return registerOnce(ctx, http.DefaultClient, gateway.URL, "", "idem-key-timeout", payload)
	})
	if err == nil {
		t.Fatal("expected timeout failure, got nil")
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (retried to exhaustion)", attempts)
	}
	if retry.IsPermanent(err) {
		t.Fatalf("timeout wrongly classified permanent: %v", err)
	}
	var netErr net.Error
	if !errors.Is(err, context.DeadlineExceeded) && !(errors.As(err, &netErr) && netErr.Timeout()) {
		t.Fatalf("error is not a timeout: %v", err)
	}
}

// probeHealth performs one GET /health, classifying non-200 as retryable.
func probeHealth(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/health", nil)
	if err != nil {
		return retry.Permanent(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health returned %d", resp.StatusCode)
	}
	return nil
}

// Failure-path matrix: vLLM exits during startup / never becomes healthy.
func TestRetryVLLMNeverHealthyExhaustsRetries(t *testing.T) {
	server := NewVLLM(VLLMOptions{FailLoad: true})
	defer server.Close()

	const maxAttempts = 4
	err := retry.Do(context.Background(), fastPolicy(maxAttempts), func(ctx context.Context) error {
		return probeHealth(ctx, server.URL)
	})
	if err == nil {
		t.Fatal("expected health polling to fail, got nil")
	}
	if retry.IsPermanent(err) {
		t.Fatalf("health failure wrongly classified permanent: %v", err)
	}
	if got := server.HealthCalls(); got != maxAttempts {
		t.Fatalf("HealthCalls = %d, want %d", got, maxAttempts)
	}
}

// Failure-path matrix: slow startup succeeds once /health turns 200.
func TestRetryVLLMSlowStartHealthySucceeds(t *testing.T) {
	server := NewVLLM(VLLMOptions{HealthyAfter: 2})
	defer server.Close()

	err := retry.Do(context.Background(), fastPolicy(5), func(ctx context.Context) error {
		return probeHealth(ctx, server.URL)
	})
	if err != nil {
		t.Fatalf("health polling failed despite recovery: %v", err)
	}
	if got := server.HealthCalls(); got != 3 {
		t.Fatalf("HealthCalls = %d, want 3 (success on third probe)", got)
	}
}

// Failure-path matrix: malformed model list is a permanent failure — a
// broken response body will not fix itself, so it must not be retried.
func TestRetryVLLMMalformedModelListIsPermanent(t *testing.T) {
	server := NewVLLM(VLLMOptions{MalformedModelList: true})
	defer server.Close()

	attempts := 0
	err := retry.Do(context.Background(), fastPolicy(5), func(ctx context.Context) error {
		attempts++
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/models", nil)
		if err != nil {
			return retry.Permanent(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		var list struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return retry.Permanent(fmt.Errorf("malformed model list: %w", err))
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected malformed model list to fail, got nil")
	}
	if !retry.IsPermanent(err) {
		t.Fatalf("malformed model list not classified permanent: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want exactly 1 for permanent error", attempts)
	}
}
