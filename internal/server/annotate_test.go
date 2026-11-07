package server

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

var note = usageNote{Provider: "cloud", UpstreamModel: "vendor/coder"}

func TestAnnotateJSONKeepsUsage(t *testing.T) {
	out := annotateJSON([]byte(`{"id":"x","usage":{"prompt_tokens":3}}`), note)
	var got struct {
		ID    string         `json:"id"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil || got.ID != "x" || got.Usage["prompt_tokens"] != 3.0 ||
		got.Usage["provider"] != "cloud" || got.Usage["upstream_model"] != "vendor/coder" {
		t.Fatalf("got %s", out)
	}
	if out := annotateJSON([]byte("not json"), note); string(out) != "not json" {
		t.Fatalf("non-JSON changed: %s", out)
	}
}

func TestAnnotateStreamUsageChunk(t *testing.T) {
	in := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"total_tokens\":9}}\n\n" +
		"data: [DONE]\n\n"
	var out bytes.Buffer
	annotateStream(&out, func() {}, strings.NewReader(in), note)
	s := out.String()
	if !strings.Contains(s, `"content":"hi"`) || strings.Count(s, `"provider":"cloud"`) != 1 ||
		!strings.Contains(s, `"total_tokens":9`) || !strings.HasSuffix(s, "data: [DONE]\n\n") {
		t.Fatalf("stream = %q", s)
	}
}

func TestAnnotateStreamWithoutUsageAddsChunk(t *testing.T) {
	in := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\ndata: [DONE]\n\n"
	var out bytes.Buffer
	annotateStream(&out, func() {}, strings.NewReader(in), note)
	s := out.String()
	i := strings.Index(s, `"provider":"cloud"`)
	if i < 0 || i > strings.Index(s, "[DONE]") || strings.Count(s, `"provider"`) != 1 {
		t.Fatalf("stream = %q", s)
	}
}
