package redact

import (
	"bytes"
	"strings"
	"testing"
)

func TestLineRedactsSecrets(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		mustHide   []string
		mustRemain []string
	}{
		{
			name:       "authorization header",
			in:         `request headers: Authorization: Bearer abc123def456 accept: application/json`,
			mustHide:   []string{"abc123def456"},
			mustRemain: []string{"Authorization:", "accept: application/json"},
		},
		{
			name:       "json authorization header",
			in:         `{"Authorization":"Bearer opaque-json-secret","accept":"application/json"}`,
			mustHide:   []string{"opaque-json-secret"},
			mustRemain: []string{`"Authorization":"`, `"accept":"application/json"`},
		},
		{
			name:       "env assignment",
			in:         `IOLD_GATEWAY_TOKEN=super-secret-value IOLD_GATEWAY_MODE=mock`,
			mustHide:   []string{"super-secret-value"},
			mustRemain: []string{"IOLD_GATEWAY_TOKEN=", "IOLD_GATEWAY_MODE=mock"},
		},
		{
			name:       "json field",
			in:         `{"api_key": "sk-proj-abcdefgh12345678", "model": "qwen"}`,
			mustHide:   []string{"sk-proj-abcdefgh12345678"},
			mustRemain: []string{`"model": "qwen"`},
		},
		{
			name:       "url credentials",
			in:         `downloading from https://user:hunter2@huggingface.co/repo`,
			mustHide:   []string{"user:hunter2"},
			mustRemain: []string{"https://", "huggingface.co/repo"},
		},
		{
			name:       "bare huggingface token",
			in:         `using token hf_AbCdEfGh123456789 for download`,
			mustHide:   []string{"hf_AbCdEfGh123456789"},
			mustRemain: []string{"using token", "for download"},
		},
		{
			name:       "cli flag",
			in:         `args: --api-key=deadbeefcafe --port 8000`,
			mustHide:   []string{"deadbeefcafe"},
			mustRemain: []string{"--port 8000"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Line(tc.in)
			for _, secret := range tc.mustHide {
				if strings.Contains(got, secret) {
					t.Fatalf("secret %q survived redaction: %q", secret, got)
				}
			}
			for _, keep := range tc.mustRemain {
				if !strings.Contains(got, keep) {
					t.Fatalf("non-secret %q was lost: %q", keep, got)
				}
			}
			if !strings.Contains(got, "[REDACTED]") {
				t.Fatalf("no redaction marker in output: %q", got)
			}
		})
	}
}

func TestLineLeavesPlainOutputAlone(t *testing.T) {
	in := "INFO 07-29 vllm engine ready on port 8000, model loaded in 42.3s"
	if got := Line(in); got != in {
		t.Fatalf("plain line was modified: %q -> %q", in, got)
	}
}

func TestCopyRedactsStream(t *testing.T) {
	src := strings.NewReader("line one\nAuthorization: Bearer topsecret\nline three\n")
	var dst bytes.Buffer
	if err := Copy(&dst, src); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	out := dst.String()
	if strings.Contains(out, "topsecret") {
		t.Fatalf("secret survived Copy: %q", out)
	}
	if !strings.Contains(out, "line one\n") || !strings.Contains(out, "line three\n") {
		t.Fatalf("non-secret lines corrupted: %q", out)
	}
}
