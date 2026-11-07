package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// usageNote is added to a response's usage object, which clients such as
// open-webui show next to each reply.
type usageNote struct {
	Provider      string `json:"provider"`
	UpstreamModel string `json:"upstream_model"`
}

func (n usageNote) addTo(obj map[string]json.RawMessage) bool {
	usage := map[string]json.RawMessage{}
	if raw, ok := obj["usage"]; ok && !bytes.Equal(raw, []byte("null")) {
		if json.Unmarshal(raw, &usage) != nil {
			return false
		}
	}
	usage["provider"], _ = json.Marshal(n.Provider)
	usage["upstream_model"], _ = json.Marshal(n.UpstreamModel)
	obj["usage"], _ = json.Marshal(usage)
	return true
}

// annotateJSON adds the note to a complete JSON response body.
func annotateJSON(body []byte, n usageNote) []byte {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil || !n.addTo(obj) {
		return body
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// annotateStream copies a server-sent event stream, adding the note to the
// chunk that carries usage, or to an extra chunk before [DONE] when none does.
func annotateStream(w io.Writer, flush func(), r io.Reader, n usageNote) {
	br := bufio.NewReaderSize(r, 64<<10)
	noted := false
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			payload, isData := bytes.CutPrefix(bytes.TrimRight(line, "\r\n"), []byte("data:"))
			payload = bytes.TrimSpace(payload)
			switch {
			case !isData:
			case bytes.Equal(payload, []byte("[DONE]")):
				if !noted {
					extra, _ := json.Marshal(map[string]any{"object": "chat.completion.chunk", "choices": []any{}, "usage": n})
					_, _ = w.Write([]byte("data: " + string(extra) + "\n\n"))
					noted = true
				}
			case bytes.Contains(payload, []byte(`"usage"`)):
				var obj map[string]json.RawMessage
				if json.Unmarshal(payload, &obj) == nil && hasValue(obj["usage"]) && n.addTo(obj) {
					if out, merr := json.Marshal(obj); merr == nil {
						line = []byte("data: " + string(out) + "\n")
						noted = true
					}
				}
			}
			if _, werr := w.Write(line); werr != nil {
				return
			}
			flush()
		}
		if err != nil {
			return
		}
	}
}

func hasValue(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

func isEventStream(h http.Header) bool {
	return strings.HasPrefix(h.Get("Content-Type"), "text/event-stream")
}

func isJSON(h http.Header) bool {
	return strings.HasPrefix(h.Get("Content-Type"), "application/json")
}
