// Package redact removes credentials from log output before it reaches
// the user (docs/ARCHITECTURE.md §8: redact tokens, Authorization headers,
// and URLs containing credentials from logs).
package redact

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
)

const placeholder = "[REDACTED]"

var rules = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	// JSON-encoded authentication headers include a quote between the field
	// name and colon, so handle them before the plain-header rule.
	{
		regexp.MustCompile(`(?i)("(?:authorization|proxy-authorization|x-api-key)"\s*:\s*")[^"]*`),
		"${1}" + placeholder,
	},
	// Authorization / X-Api-Key style headers, keeping the header name.
	{
		regexp.MustCompile(`(?i)\b(authorization|proxy-authorization|x-api-key)(\s*[:=]\s*)\S+(\s+\S+)?`),
		"${1}${2}" + placeholder,
	},
	// KEY=value and key: value assignments whose name suggests a secret,
	// covering env dumps, CLI args, and JSON-ish log lines.
	{
		regexp.MustCompile(`(?i)([\w.-]*(?:token|secret|password|passwd|credential|api[_-]?key)[\w.-]*"?\s*[:=]\s*"?)[^\s",;&]+`),
		"${1}" + placeholder,
	},
	// URL userinfo: scheme://user:pass@host
	{
		regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s@]+@`),
		"${1}" + placeholder + "@",
	},
	// Well-known token shapes that can appear bare in output.
	{
		regexp.MustCompile(`\b(?:hf_|sk-|ghp_|gho_|glpat-|xox[a-z]-)[A-Za-z0-9_-]{8,}`),
		placeholder,
	},
}

// Line redacts credentials in a single log line.
func Line(line string) string {
	for _, rule := range rules {
		line = rule.pattern.ReplaceAllString(line, rule.replacement)
	}
	return line
}

// Copy streams src to dst, redacting line by line.
func Copy(dst io.Writer, src io.Reader) error {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if _, err := fmt.Fprintln(dst, Line(scanner.Text())); err != nil {
			return err
		}
	}
	return scanner.Err()
}
