package openai

import (
	"strings"
	"testing"
)

func TestDecodeRejectsInvalidControls(t *testing.T) {
	for _, fields := range []string{
		`"max_tokens":-1`, `"max_completion_tokens":-1`, `"n":-1`,
		`"temperature":-1`, `"temperature":3`, `"top_p":1.1`,
		`"router":{"max_cost_usd":-1}`, `"router":{"max_latency_ms":-1}`,
		`"router":{"min_quality":1.1}`, `"reasoning_effort":"typo"`,
		`"tools":[{"type":"web_search"}]`, `"response_format":{"type":"typo"}`,
		`"tool_choice":"required"`,
		`"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":{"type":"function","function":{"name":"missing"}}`,
	} {
		t.Run(fields, func(t *testing.T) {
			_, err := Decode(strings.NewReader(`{"messages":[{"role":"user","content":"hi"}],` + fields + `}`))
			if err == nil {
				t.Fatal("invalid control accepted")
			}
		})
	}
}

func TestDecodeRequiresSingleJSONValue(t *testing.T) {
	for _, suffix := range []string{` {}`, ` garbage`} {
		if _, err := Decode(strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}` + suffix)); err == nil {
			t.Fatal("trailing data accepted")
		}
	}
}

func TestDecodeRejectsMalformedAudio(t *testing.T) {
	for _, part := range []string{`{"type":"input_audio"}`, `{"type":"input_audio","input_audio":{"format":"wav"}}`, `{"type":"input_audio","input_audio":{"data":"abc","format":"unknown"}}`} {
		if _, err := Decode(strings.NewReader(`{"messages":[{"role":"user","content":[` + part + `]}]}`)); err == nil {
			t.Fatal("malformed audio accepted")
		}
	}
}
