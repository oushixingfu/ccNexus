package cc

import (
	"github.com/lich0821/ccNexus/internal/transformer"
	"github.com/lich0821/ccNexus/internal/transformer/convert"
)

// OpenAI2Transformer transforms Claude Code requests to OpenAI Responses API format
type OpenAI2Transformer struct {
	model    string
	thinking string
}

// NewOpenAI2Transformer creates a new transformer
func NewOpenAI2Transformer(model string) *OpenAI2Transformer {
	return NewOpenAI2TransformerWithThinking(model, "")
}

// NewOpenAI2TransformerWithThinking creates a new transformer with endpoint-level reasoning effort.
func NewOpenAI2TransformerWithThinking(model, thinking string) *OpenAI2Transformer {
	return &OpenAI2Transformer{model: model, thinking: thinking}
}

func (t *OpenAI2Transformer) Name() string {
	return "cc_openai2"
}

func (t *OpenAI2Transformer) TransformRequest(req []byte) ([]byte, error) {
	return convert.ClaudeReqToOpenAI2WithThinking(req, t.model, t.thinking)
}

func (t *OpenAI2Transformer) TransformResponse(resp []byte, isStreaming bool) ([]byte, error) {
	if isStreaming {
		return nil, nil
	}
	return convert.OpenAI2RespToClaude(resp)
}

func (t *OpenAI2Transformer) TransformResponseWithContext(resp []byte, isStreaming bool, ctx *transformer.StreamContext) ([]byte, error) {
	if isStreaming {
		return convert.OpenAI2StreamToClaude(resp, ctx)
	}
	return convert.OpenAI2RespToClaude(resp)
}
