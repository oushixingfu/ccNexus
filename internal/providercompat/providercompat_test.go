package providercompat

import (
	"encoding/json"
	"testing"
)

func TestNormalizeTargetPathForVersionedBaseURL(t *testing.T) {
	got := NormalizeTargetPathForBaseURL("https://api.moonshot.ai/v1", "/v1/chat/completions")
	if got != "/chat/completions" {
		t.Fatalf("expected /chat/completions, got %s", got)
	}
}

func TestNormalizeTargetPathForFullURL(t *testing.T) {
	got := NormalizeTargetPathForBaseURL("https://api.example.com/v1/chat/completions", "/v1/chat/completions")
	if got != "" {
		t.Fatalf("expected empty target path for full URL, got %s", got)
	}
}

func TestBuildOpenAIModelURLCandidatesDeepSeek(t *testing.T) {
	got, err := BuildOpenAIModelURLCandidates("https://api.deepseek.com/anthropic", "deepseek")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{
		"https://api.deepseek.com/models",
		"https://api.deepseek.com/anthropic/v1/models",
		"https://api.deepseek.com/anthropic/models",
		"https://api.deepseek.com/v1/models",
	}
	for _, expected := range want {
		if !contains(got, expected) {
			t.Fatalf("expected candidates to contain %s, got %#v", expected, got)
		}
	}
}

func TestBuildOpenAIModelURLCandidatesDeepSeekCustomGateway(t *testing.T) {
	got, err := BuildOpenAIModelURLCandidates("https://gateway.example.com", "deepseek")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got[0] != "https://gateway.example.com/v1/models" {
		t.Fatalf("expected custom DeepSeek gateway to prefer /v1/models, got %#v", got)
	}
	if got[1] != "https://gateway.example.com/models" {
		t.Fatalf("expected /models fallback, got %#v", got)
	}
}

func TestBuildOpenAIModelURLCandidatesVersionedBase(t *testing.T) {
	got, err := BuildOpenAIModelURLCandidates("https://api.moonshot.ai/v1", "kimi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got[0] != "https://api.moonshot.ai/v1/models" {
		t.Fatalf("expected first candidate to avoid duplicated v1, got %#v", got)
	}
}

func TestResolveLoopbackBaseURLForContainer(t *testing.T) {
	got := resolveLoopbackBaseURLForContainer("http://127.0.0.1:6011/v1", true)
	if got != "http://host.docker.internal:6011/v1" {
		t.Fatalf("expected host.docker.internal rewrite, got %s", got)
	}

	got = resolveLoopbackBaseURLForContainer("http://localhost:8000/v1", true)
	if got != "http://host.docker.internal:8000/v1" {
		t.Fatalf("expected localhost rewrite, got %s", got)
	}

	got = resolveLoopbackBaseURLForContainer("http://127.0.0.1:6011/v1", false)
	if got != "http://127.0.0.1:6011/v1" {
		t.Fatalf("expected disabled rewrite to preserve URL, got %s", got)
	}
}

func TestOpenAIChatTargetPathDeepSeekCustomGateway(t *testing.T) {
	got := OpenAIChatTargetPath("deepseek", "https://gateway.example.com")
	if got != "/v1/chat/completions" {
		t.Fatalf("expected /v1/chat/completions for custom DeepSeek gateway, got %s", got)
	}
}

func TestOpenAIChatTargetPathDeepSeekOfficial(t *testing.T) {
	got := OpenAIChatTargetPath("deepseek", "https://api.deepseek.com")
	if got != "/chat/completions" {
		t.Fatalf("expected /chat/completions for official DeepSeek API, got %s", got)
	}
}

func TestInferEndpointTransformerFromKimiModel(t *testing.T) {
	got := InferEndpointTransformer("https://gateway.example.com", "kimi-k2.6", "auto")
	if got != TransformerKimi {
		t.Fatalf("expected kimi transformer from model, got %s", got)
	}
}

func TestInferEndpointTransformerPreservesExplicitTransformer(t *testing.T) {
	got := InferEndpointTransformer("https://gateway.example.com", "kimi-k2.6", "openai2")
	if got != TransformerOpenAI2 {
		t.Fatalf("expected explicit openai2 to be preserved, got %s", got)
	}
}

func TestInferEndpointTransformerFromGPTModelUsesResponses(t *testing.T) {
	got := InferEndpointTransformer("https://gateway.example.com", "gpt-5.5", "auto")
	if got != TransformerOpenAI2 {
		t.Fatalf("expected gpt model to use openai2, got %s", got)
	}
}

func TestInferEndpointTransformerDefaultsUnknownURLToOpenAI(t *testing.T) {
	got := InferEndpointTransformer("https://gateway.example.com", "", "auto")
	if got != TransformerOpenAI {
		t.Fatalf("expected unknown custom gateway to default to openai, got %s", got)
	}
}

func TestAdaptOpenAIChatPayloadForDeepSeek(t *testing.T) {
	raw := []byte(`{"model":"deepseek-chat","max_completion_tokens":8,"reasoning":{"effort":"medium"}}`)
	out := AdaptOpenAIChatPayload(raw, "deepseek", "https://api.deepseek.com", "")

	var payload map[string]interface{}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if _, ok := payload["max_completion_tokens"]; ok {
		t.Fatalf("did not expect max_completion_tokens, got %#v", payload)
	}
	if payload["max_tokens"].(float64) != 8 {
		t.Fatalf("expected max_tokens=8, got %#v", payload["max_tokens"])
	}
	if payload["reasoning_effort"] != "high" {
		t.Fatalf("expected reasoning_effort=high, got %#v", payload["reasoning_effort"])
	}
	thinking, ok := payload["thinking"].(map[string]interface{})
	if !ok || thinking["type"] != "enabled" {
		t.Fatalf("expected thinking.type=enabled, got %#v", payload["thinking"])
	}
	if _, ok := payload["reasoning"]; ok {
		t.Fatalf("did not expect reasoning object, got %#v", payload["reasoning"])
	}
}

func TestAdaptOpenAIChatPayloadForDeepSeekDefaultLeavesThinkingUnset(t *testing.T) {
	raw := []byte(`{"model":"deepseek-chat","messages":[]}`)
	out := AdaptOpenAIChatPayload(raw, "deepseek", "https://api.deepseek.com", "")

	var payload map[string]interface{}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if _, ok := payload["reasoning_effort"]; ok {
		t.Fatalf("did not expect reasoning_effort for provider default, got %#v", payload["reasoning_effort"])
	}
	if _, ok := payload["thinking"]; ok {
		t.Fatalf("did not expect thinking for provider default, got %#v", payload["thinking"])
	}
}

func TestAdaptOpenAIChatPayloadForDeepSeekThinkingOff(t *testing.T) {
	raw := []byte(`{"model":"deepseek-chat","messages":[],"reasoning_effort":"max"}`)
	out := AdaptOpenAIChatPayload(raw, "deepseek", "https://api.deepseek.com", "off")

	var payload map[string]interface{}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if _, ok := payload["reasoning_effort"]; ok {
		t.Fatalf("did not expect reasoning_effort when thinking is off, got %#v", payload["reasoning_effort"])
	}
	thinking, ok := payload["thinking"].(map[string]interface{})
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("expected thinking.type=disabled, got %#v", payload["thinking"])
	}
}

func TestAdaptOpenAIChatPayloadForDeepSeekXHighMapsToMax(t *testing.T) {
	raw := []byte(`{"model":"deepseek-chat","messages":[]}`)
	out := AdaptOpenAIChatPayload(raw, "deepseek", "https://api.deepseek.com", "xhigh")

	var payload map[string]interface{}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if payload["reasoning_effort"] != "max" {
		t.Fatalf("expected reasoning_effort=max, got %#v", payload["reasoning_effort"])
	}
	thinking, ok := payload["thinking"].(map[string]interface{})
	if !ok || thinking["type"] != "enabled" {
		t.Fatalf("expected thinking.type=enabled, got %#v", payload["thinking"])
	}
}

func TestAdaptOpenAIChatPayloadForKimiConvertsDeveloperRole(t *testing.T) {
	raw := []byte(`{"model":"kimi-k2.6","messages":[{"role":"developer","content":"policy"},{"role":"user","content":"hi"}]}`)
	out := AdaptOpenAIChatPayload(raw, "kimi", "https://1052.cc.cd:5005", "")

	var payload map[string]interface{}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	messages, ok := payload["messages"].([]interface{})
	if !ok || len(messages) != 2 {
		t.Fatalf("expected two messages, got %#v", payload["messages"])
	}
	first, ok := messages[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected first message object, got %#v", messages[0])
	}
	if first["role"] != "system" {
		t.Fatalf("expected developer role to be converted to system, got %#v", first["role"])
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
