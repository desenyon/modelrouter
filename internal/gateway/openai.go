// Package gateway serves the OpenAI-compatible modelrouter HTTP API.
package gateway

import (
	"encoding/json"
	"time"
)

// ChatRequest is a subset of the OpenAI chat completions schema.
type ChatRequest struct {
	Model            string          `json:"model"`
	Messages         []ChatMessage   `json:"messages"`
	Tools            json.RawMessage `json:"tools,omitempty"`
	Stream           bool            `json:"stream,omitempty"`
	ResponseFormat   json.RawMessage `json:"response_format,omitempty"`
	OptimizeFor      string          `json:"optimize_for,omitempty"`
}

// ChatMessage is a single chat turn.
type ChatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// contentText flattens OpenAI string or multipart content to plain text.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err != nil {
		return string(raw)
	}
	var out string
	for _, p := range parts {
		if t, _ := p["type"].(string); t == "text" {
			if txt, ok := p["text"].(string); ok {
				out += txt + "\n"
			}
		}
	}
	return out
}

func hasImage(raw json.RawMessage) bool {
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err != nil {
		return false
	}
	for _, p := range parts {
		if t, _ := p["type"].(string); t == "image_url" || t == "input_image" {
			return true
		}
	}
	return false
}

func toolCount(raw json.RawMessage) int {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return 0
	}
	return len(arr)
}

func isStructured(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	t, _ := obj["type"].(string)
	return t == "json_schema" || t == "json_object"
}

// ModelList is an OpenAI-style /v1/models response.
type ModelList struct {
	Object string       `json:"object"`
	Data   []ModelEntry `json:"data"`
}

// ModelEntry is one listed model.
type ModelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// RoutePreview is returned by POST /v1/route without calling upstream.
type RoutePreview struct {
	Decision any       `json:"decision"`
	At       time.Time `json:"at"`
}

// ErrorBody matches OpenAI error envelopes.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the nested error object.
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}
