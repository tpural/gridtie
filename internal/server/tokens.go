package server

import "encoding/json"

// charsPerToken is deliberately low: code and non-English text take more
// tokens per character than prose, and overestimating only costs a fallback.
const charsPerToken = 3

// estimateTokens guesses how much context a request needs: its text, tools and
// requested output. Images and other non-text parts are not counted.
func estimateTokens(fields map[string]json.RawMessage) int {
	chars := len(fields["tools"])
	var messages []struct {
		Content json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(fields["messages"], &messages)
	for _, m := range messages {
		chars += textLen(m.Content) + 4*charsPerToken
	}
	chars += textLen(fields["prompt"])
	var maxTokens, maxCompletionTokens int
	_ = json.Unmarshal(fields["max_tokens"], &maxTokens)
	_ = json.Unmarshal(fields["max_completion_tokens"], &maxCompletionTokens)
	return (chars+charsPerToken-1)/charsPerToken + max(maxTokens, maxCompletionTokens)
}

// textLen counts the text in a string, a list of strings, or a list of content
// parts like {"type":"text","text":"..."}.
func textLen(raw json.RawMessage) int {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return len(s)
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return 0
	}
	n := 0
	for _, p := range parts {
		var part struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(p, &s) == nil {
			n += len(s)
		} else if json.Unmarshal(p, &part) == nil {
			n += len(part.Text)
		}
	}
	return n
}
