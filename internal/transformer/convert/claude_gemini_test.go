package convert

import (
	"encoding/json"
	"testing"
)

func TestClaudeReqToGeminiPreservesZeroTemperature(t *testing.T) {
	claudeReq := `{
		"model": "claude-3-opus-20240229",
		"messages": [{"role": "user", "content": "Hello"}],
		"max_tokens": 1024,
		"temperature": 0
	}`

	geminiReqBytes, err := ClaudeReqToGemini([]byte(claudeReq), "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("ClaudeReqToGemini failed: %v", err)
	}

	var geminiReq map[string]interface{}
	if err := json.Unmarshal(geminiReqBytes, &geminiReq); err != nil {
		t.Fatalf("Failed to unmarshal Gemini request: %v", err)
	}

	genConfig, ok := geminiReq["generationConfig"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected generationConfig, got %#v", geminiReq["generationConfig"])
	}

	temperature, ok := genConfig["temperature"].(float64)
	if !ok {
		t.Fatalf("expected explicit temperature=0 to be preserved, got %#v", genConfig["temperature"])
	}
	if temperature != 0 {
		t.Fatalf("expected temperature=0, got %v", temperature)
	}
}
