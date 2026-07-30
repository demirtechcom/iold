package fakes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, url, apiKey string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

func TestVLLMDelayedHealth(t *testing.T) {
	server := NewVLLM(VLLMOptions{HealthyAfter: 2})
	defer server.Close()
	for i, want := range []int{503, 503, 200, 200} {
		resp, _ := get(t, server.URL+"/health", "")
		if resp.StatusCode != want {
			t.Fatalf("health call %d = %d, want %d", i+1, resp.StatusCode, want)
		}
	}
	if server.HealthCalls() != 4 {
		t.Fatalf("HealthCalls = %d, want 4", server.HealthCalls())
	}
}

func TestVLLMFailedLoad(t *testing.T) {
	server := NewVLLM(VLLMOptions{FailLoad: true})
	defer server.Close()
	for range 3 {
		if resp, _ := get(t, server.URL+"/health", ""); resp.StatusCode != 500 {
			t.Fatalf("failed-load health = %d, want 500", resp.StatusCode)
		}
	}
}

func TestVLLMModelListAndAuth(t *testing.T) {
	server := NewVLLM(VLLMOptions{Model: "qwen3.6-35b-a3b", APIKey: "k1"})
	defer server.Close()

	if resp, _ := get(t, server.URL+"/v1/models", ""); resp.StatusCode != 401 {
		t.Fatalf("unauthenticated models = %d, want 401", resp.StatusCode)
	}
	if resp, _ := get(t, server.URL+"/v1/models", "wrong"); resp.StatusCode != 401 {
		t.Fatalf("wrong-key models = %d, want 401", resp.StatusCode)
	}
	resp, body := get(t, server.URL+"/v1/models", "k1")
	if resp.StatusCode != 200 {
		t.Fatalf("models = %d, want 200", resp.StatusCode)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("model list is not valid JSON: %v", err)
	}
	if len(list.Data) != 1 || list.Data[0].ID != "qwen3.6-35b-a3b" {
		t.Fatalf("unexpected model list: %s", body)
	}
}

func TestVLLMMalformedModelList(t *testing.T) {
	server := NewVLLM(VLLMOptions{MalformedModelList: true})
	defer server.Close()
	_, body := get(t, server.URL+"/v1/models", "")
	var anything any
	if err := json.Unmarshal([]byte(body), &anything); err == nil {
		t.Fatalf("expected malformed JSON, got parseable: %s", body)
	}
}

func chat(t *testing.T, server *VLLM, payload string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", bytes.NewBufferString(payload))
	if err != nil {
		t.Fatalf("POST chat: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

func TestVLLMDeterministicInference(t *testing.T) {
	server := NewVLLM(VLLMOptions{Model: "m"})
	defer server.Close()

	resp, body := chat(t, server, `{"model": "m", "messages": []}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"pong"`) {
		t.Fatalf("chat = %d %s", resp.StatusCode, body)
	}
	if resp, _ := chat(t, server, `{"model": "other", "messages": []}`); resp.StatusCode != 404 {
		t.Fatalf("unknown model chat = %d, want 404", resp.StatusCode)
	}
}

func TestVLLMStreaming(t *testing.T) {
	server := NewVLLM(VLLMOptions{Model: "m"})
	defer server.Close()

	resp, body := chat(t, server, `{"model": "m", "stream": true}`)
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q", got)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream missing [DONE]: %q", body)
	}
	if content := ReadSSEContent(body); content != "pong" {
		t.Fatalf("streamed content = %q, want pong", content)
	}
}

func register(t *testing.T, gw *Gateway, token, key, payload string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/internal/model-endpoints", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+token)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

const validRegistration = `{
	"deployment_id": "dep-1", "model": "qwen3.6-35b-a3b",
	"base_url": "http://127.0.0.1:8000", "api_key": "vk", "runtime": "vllm"
}`

func TestGatewayRegisterIdempotentReplayAndConflict(t *testing.T) {
	gw := NewGateway(GatewayOptions{Token: "gt"})
	defer gw.Close()

	resp, first := register(t, gw, "gt", "idem-1", validRegistration)
	if resp.StatusCode != 201 {
		t.Fatalf("register = %d %s", resp.StatusCode, first)
	}
	resp, replay := register(t, gw, "gt", "idem-1", validRegistration)
	if resp.StatusCode != 201 || replay != first {
		t.Fatalf("idempotent replay = %d %s, want 201 with identical body %s", resp.StatusCode, replay, first)
	}
	if gw.RegisterCalls() != 2 {
		t.Fatalf("RegisterCalls = %d, want 2", gw.RegisterCalls())
	}
	if resp, _ := register(t, gw, "gt", "idem-2", validRegistration); resp.StatusCode != 409 {
		t.Fatalf("conflicting register = %d, want 409", resp.StatusCode)
	}
}

func TestGatewayAuthAndValidation(t *testing.T) {
	gw := NewGateway(GatewayOptions{Token: "gt"})
	defer gw.Close()

	if resp, _ := register(t, gw, "wrong", "k", validRegistration); resp.StatusCode != 401 {
		t.Fatalf("bad token = %d, want 401", resp.StatusCode)
	}
	if resp, _ := register(t, gw, "gt", "", validRegistration); resp.StatusCode != 400 {
		t.Fatalf("missing idempotency key = %d, want 400", resp.StatusCode)
	}
	if resp, _ := register(t, gw, "gt", "k", `{"deployment_id": "dep-1"}`); resp.StatusCode != 400 {
		t.Fatalf("missing fields = %d, want 400", resp.StatusCode)
	}
}

func TestGatewayUnavailableThenRecovers(t *testing.T) {
	gw := NewGateway(GatewayOptions{UnavailableFor: 2})
	defer gw.Close()

	for i := range 2 {
		if resp, _ := register(t, gw, "any", fmt.Sprintf("k%d", i), validRegistration); resp.StatusCode != 503 {
			t.Fatalf("call %d = %d, want 503", i+1, resp.StatusCode)
		}
	}
	if resp, _ := register(t, gw, "any", "k-final", validRegistration); resp.StatusCode != 201 {
		t.Fatalf("post-recovery register = %d, want 201", resp.StatusCode)
	}
	if !gw.Registered("dep-1") {
		t.Fatal("registration missing after recovery")
	}
}

func TestGatewayInspectAndUnregister(t *testing.T) {
	gw := NewGateway(GatewayOptions{})
	defer gw.Close()
	register(t, gw, "any", "k", validRegistration)

	resp, body := get(t, gw.URL+"/internal/model-endpoints/dep-1", "any")
	if resp.StatusCode != 200 || !strings.Contains(body, `"deployment_id":"dep-1"`) {
		t.Fatalf("inspect = %d %s", resp.StatusCode, body)
	}
	if resp, _ := get(t, gw.URL+"/internal/model-endpoints/ghost", "any"); resp.StatusCode != 404 {
		t.Fatalf("inspect missing = %d, want 404", resp.StatusCode)
	}

	del := func(id string) int {
		req, _ := http.NewRequest(http.MethodDelete, gw.URL+"/internal/model-endpoints/"+id, nil)
		req.Header.Set("Authorization", "Bearer any")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := del("dep-1"); code != 204 {
		t.Fatalf("unregister = %d, want 204", code)
	}
	if gw.Registered("dep-1") {
		t.Fatal("still registered after unregister")
	}
	if code := del("dep-1"); code != 204 {
		t.Fatalf("unregister absent = %d, want 204 (idempotent)", code)
	}
}

func TestGatewayUnregisterFailure(t *testing.T) {
	gw := NewGateway(GatewayOptions{FailUnregister: true})
	defer gw.Close()
	register(t, gw, "any", "k", validRegistration)

	req, _ := http.NewRequest(http.MethodDelete, gw.URL+"/internal/model-endpoints/dep-1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("failing unregister = %d, want 500", resp.StatusCode)
	}
	if !gw.Registered("dep-1") {
		t.Fatal("registration should survive failed unregister")
	}
}

func TestGatewayResponseDelaySupportsTimeoutTests(t *testing.T) {
	gw := NewGateway(GatewayOptions{ResponseDelay: 200 * time.Millisecond})
	defer gw.Close()

	client := &http.Client{Timeout: 30 * time.Millisecond}
	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/internal/model-endpoints/dep-1", nil)
	if _, err := client.Do(req); err == nil {
		t.Fatal("expected client timeout against delayed gateway")
	}
}
