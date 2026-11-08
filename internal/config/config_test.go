package config

import (
	"strings"
	"testing"
	"time"
)

const valid = `
providers:
  local:
    baseURL: http://llama:8080/v1
    readiness: {llamaSwap: http://llama:8080}
    wake: {url: http://gate:9090/wake}
  cloud:
    baseURL: https://openrouter.ai/api/v1
    apiKeyEnv: TEST_KEY
models:
  coder:
    aliases: [coding]
    routes:
      - {provider: local, model: coder}
      - {provider: cloud, model: vendor/coder}
rules:
  - {header: "User-Agent=^n8n", localOnly: true}
`

func TestParseDefaults(t *testing.T) {
	t.Setenv("TEST_KEY", "k")
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.AdminListen != ":9090" || c.Cooldown.Duration != 10*time.Minute {
		t.Fatalf("defaults = %q %q %v", c.Listen, c.AdminListen, c.Cooldown)
	}
}

func TestParseErrors(t *testing.T) {
	tests := map[string]struct{ edit func(string) string }{
		"is not set":       {func(s string) string { return s }},
		"unknown provider": {func(s string) string { return strings.Replace(s, "provider: cloud", "provider: nope", 1) }},
		"also used by":     {func(s string) string { return strings.Replace(s, "[coding]", "[coder]", 1) }},
		"exactly one":      {func(s string) string { return strings.Replace(s, `{header:`, `{cidr: 10.0.0.0/8, header:`, 1) }},
		"is not a URL":     {func(s string) string { return strings.Replace(s, "http://llama:8080/v1", "llama", 1) }},
		"needs llamaSwap": {func(s string) string {
			return strings.Replace(s, "apiKeyEnv: TEST_KEY", "apiKeyEnv: TEST_KEY\n    readiness: {slots: true}", 1)
		}},
		"negative maxContext": {func(s string) string {
			return strings.Replace(s, "model: vendor/coder}", "model: vendor/coder, maxContext: -1}", 1)
		}},
		`unknown field "tyop"`: {func(s string) string { return s + "tyop: 1\n" }},
	}
	for want, tt := range tests {
		t.Run(want, func(t *testing.T) {
			if want != "is not set" {
				t.Setenv("TEST_KEY", "k")
			}
			_, err := Parse([]byte(tt.edit(valid)))
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want %q", err, want)
			}
		})
	}
}
