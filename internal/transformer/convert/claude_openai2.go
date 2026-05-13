package convert

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/transformer"
)

// ClaudeReqToOpenAI2 converts Claude request to OpenAI Responses API request
func ClaudeReqToOpenAI2(claudeReq []byte, model string) ([]byte, error) {
	return ClaudeReqToOpenAI2WithThinking(claudeReq, model, "")
}

// ClaudeReqToOpenAI2WithThinking converts Claude request to OpenAI Responses API request
// and injects endpoint-level reasoning effort when configured.
func ClaudeReqToOpenAI2WithThinking(claudeReq []byte, model string, thinking string) ([]byte, error) {
	return ClaudeReqToOpenAI2WithThinkingAndAPIURL(claudeReq, model, thinking, "")
}

// ClaudeReqToOpenAI2WithThinkingAndAPIURL converts Claude request to OpenAI Responses API request
// and injects endpoint-level reasoning effort using upstream-specific field semantics.
func ClaudeReqToOpenAI2WithThinkingAndAPIURL(claudeReq []byte, model string, thinking string, apiURL string) ([]byte, error) {
	var req transformer.ClaudeRequest
	if err := json.Unmarshal(claudeReq, &req); err != nil {
		return nil, err
	}

	openai2Req := map[string]interface{}{
		"model":  model,
		"stream": req.Stream,
	}
	if req.Temperature != nil {
		openai2Req["temperature"] = *req.Temperature
	}
	if field, level := config.OpenAI2ThinkingField(apiURL, model, thinking); level != "" {
		if field == "effortLevel" {
			openai2Req["effortLevel"] = level
		} else {
			openai2Req["reasoning"] = map[string]interface{}{"effort": level}
		}
	}

	// Convert system to instructions
	if req.System != nil {
		openai2Req["instructions"] = extractSystemText(req.System)
	}

	// Convert messages to input
	var input []map[string]interface{}
	for _, msg := range req.Messages {
		switch content := msg.Content.(type) {
		case string:
			textType := "input_text"
			if msg.Role == "assistant" {
				textType = "output_text"
			}
			input = append(input, map[string]interface{}{
				"type": "message",
				"role": msg.Role,
				"content": []map[string]interface{}{
					{
						"type": textType,
						"text": content,
					},
				},
			})
		case []interface{}:
			input = append(input, convertClaudeMessageToOpenAI2Items(content, msg.Role)...)
		}
	}
	openai2Req["input"] = input

	// TODO: max_output_tokens is standard OpenAI Responses API param but some
	// third-party endpoints (e.g. SiliconFlow) don't support it. Skipping for compatibility.

	// Convert tools
	if len(req.Tools) > 0 {
		var tools []map[string]interface{}
		for _, tool := range req.Tools {
			tools = append(tools, map[string]interface{}{
				"type":        "function",
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  tool.InputSchema,
			})
		}
		openai2Req["tools"] = tools

		// Preserve tool forcing semantics for Responses API backends.
		if mapped := mapClaudeToolChoiceToOpenAI2(req.ToolChoice); mapped != nil {
			openai2Req["tool_choice"] = mapped
		} else {
			// For first turn, prefer required to avoid "plan-only" responses.
			// After at least one tool_result exists, switch to auto to prevent
			// forced repeated tool calls in later turns.
			if hasClaudeToolResult(req.Messages) {
				openai2Req["tool_choice"] = "auto"
			} else {
				openai2Req["tool_choice"] = "required"
			}
		}
	}

	return json.Marshal(openai2Req)
}

func mapClaudeToolChoiceToOpenAI2(toolChoice interface{}) interface{} {
	if toolChoice == nil {
		return nil
	}

	switch tc := toolChoice.(type) {
	case map[string]interface{}:
		choiceType, _ := tc["type"].(string)
		switch choiceType {
		case "tool":
			if name, ok := tc["name"].(string); ok && name != "" {
				return map[string]interface{}{
					"type": "function",
					"name": name,
				}
			}
		case "any":
			return "required"
		case "auto":
			return "auto"
		case "none":
			return "none"
		}
	case string:
		switch tc {
		case "any":
			return "required"
		default:
			return tc
		}
	}

	return nil
}

func hasClaudeToolResult(messages []transformer.ClaudeMessage) bool {
	for _, msg := range messages {
		blocks, ok := msg.Content.([]interface{})
		if !ok {
			continue
		}
		for _, block := range blocks {
			m, ok := block.(map[string]interface{})
			if !ok {
				continue
			}
			if t, _ := m["type"].(string); t == "tool_result" {
				return true
			}
		}
	}
	return false
}

// OpenAI2ReqToClaude converts OpenAI Responses API request to Claude request
func OpenAI2ReqToClaude(openai2Req []byte, model string) ([]byte, error) {
	var req transformer.OpenAI2Request
	if err := json.Unmarshal(openai2Req, &req); err != nil {
		return nil, err
	}

	claudeReq := map[string]interface{}{
		"model":      model,
		"max_tokens": 8192,
		"stream":     req.Stream,
	}

	if req.Instructions != "" {
		claudeReq["system"] = req.Instructions
	}
	if req.MaxOutputTokens > 0 {
		claudeReq["max_tokens"] = req.MaxOutputTokens
	}
	if req.Temperature != nil {
		claudeReq["temperature"] = *req.Temperature
	}

	// Convert input to messages
	messages := convertOpenAI2InputToClaude(req.Input)
	claudeReq["messages"] = messages

	// Convert tools
	if len(req.Tools) > 0 {
		var tools []map[string]interface{}
		for _, tool := range req.Tools {
			var inputSchema map[string]interface{}
			switch tool.Type {
			case "function":
				inputSchema = tool.Parameters
			case "custom":
				inputSchema = map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"input": map[string]interface{}{"type": "string", "description": "The input for this tool"},
					},
					"required": []string{"input"},
				}
			default:
				continue
			}
			tools = append(tools, map[string]interface{}{
				"name":         tool.Name,
				"description":  tool.Description,
				"input_schema": inputSchema,
			})
		}
		if len(tools) > 0 {
			claudeReq["tools"] = tools
		}
	}

	return json.Marshal(claudeReq)
}

// ClaudeRespToOpenAI2 converts Claude response to OpenAI Responses API response
func ClaudeRespToOpenAI2(claudeResp []byte) ([]byte, error) {
	var resp transformer.ClaudeResponse
	if err := json.Unmarshal(claudeResp, &resp); err != nil {
		return nil, err
	}

	var outputContent []map[string]interface{}
	var functionCalls []map[string]interface{}

	for _, block := range resp.Content {
		blockMap, ok := block.(map[string]interface{})
		if !ok {
			continue
		}
		switch blockMap["type"] {
		case "text":
			outputContent = append(outputContent, map[string]interface{}{
				"type": "output_text",
				"text": blockMap["text"],
			})
		case "thinking":
			// Skip thinking blocks in response
			continue
		case "tool_use":
			args, _ := json.Marshal(blockMap["input"])
			functionCalls = append(functionCalls, map[string]interface{}{
				"type":      "function_call",
				"id":        blockMap["id"],
				"call_id":   blockMap["id"],
				"name":      blockMap["name"],
				"arguments": string(args),
			})
		}
	}

	var output []map[string]interface{}
	if len(outputContent) > 0 {
		output = append(output, map[string]interface{}{
			"type":    "message",
			"role":    "assistant",
			"content": outputContent,
		})
	}
	output = append(output, functionCalls...)

	openai2Resp := map[string]interface{}{
		"id":     resp.ID,
		"object": "response",
		"status": "completed",
		"output": output,
		"usage": map[string]interface{}{
			"input_tokens":  resp.Usage.InputTokens,
			"output_tokens": resp.Usage.OutputTokens,
			"total_tokens":  resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}

	return json.Marshal(openai2Resp)
}

// OpenAI2RespToClaude converts OpenAI Responses API response to Claude response
func OpenAI2RespToClaude(openai2Resp []byte) ([]byte, error) {
	var resp struct {
		ID     string                   `json:"id"`
		Output []map[string]interface{} `json:"output"`
		Usage  struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(openai2Resp, &resp); err != nil {
		return nil, err
	}

	var content []map[string]interface{}
	stopReason := "end_turn"

	for _, item := range resp.Output {
		itemType := strings.ToLower(strings.TrimSpace(stringFromMap(item, "type")))
		switch itemType {
		case "message":
			content = append(content, openAI2MessageItemToClaudeContent(item)...)
		case "function_call":
			content = append(content, openAI2ToolItemToClaudeBlock(item))
			stopReason = "tool_use"
		default:
			if isOpenAI2ToolOutputType(itemType) {
				content = append(content, openAI2ToolItemToClaudeBlock(item))
				stopReason = "tool_use"
			}
		}
	}

	claudeResp := map[string]interface{}{
		"id":          resp.ID,
		"type":        "message",
		"role":        "assistant",
		"content":     content,
		"stop_reason": stopReason,
		"usage": map[string]interface{}{
			"input_tokens":  resp.Usage.InputTokens,
			"output_tokens": resp.Usage.OutputTokens,
		},
	}

	return json.Marshal(claudeResp)
}

func openAI2MessageItemToClaudeContent(item map[string]interface{}) []map[string]interface{} {
	var content []map[string]interface{}
	if text := strings.TrimSpace(stringFromMap(item, "content")); text != "" {
		return append(content, splitThinkTaggedText(text)...)
	}
	parts, ok := item["content"].([]interface{})
	if !ok {
		return content
	}
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]interface{})
		if !ok {
			continue
		}
		partType := strings.ToLower(strings.TrimSpace(stringFromMap(part, "type")))
		if partType != "output_text" && partType != "input_text" && partType != "text" {
			continue
		}
		if text := stringFromMap(part, "text"); strings.TrimSpace(text) != "" {
			content = append(content, splitThinkTaggedText(text)...)
		}
	}
	return content
}

func openAI2ToolItemToClaudeBlock(item map[string]interface{}) map[string]interface{} {
	itemType := strings.ToLower(strings.TrimSpace(stringFromMap(item, "type")))
	toolID := firstStringFromMap(item, "call_id", "id")
	if toolID == "" {
		toolID = "call_" + strings.ReplaceAll(itemType, "-", "_")
	}
	name := firstStringFromMap(item, "name")
	if name == "" {
		name = itemType
	}
	if name == "" {
		name = "tool"
	}
	return map[string]interface{}{
		"type":  "tool_use",
		"id":    toolID,
		"name":  name,
		"input": openAI2ToolItemInput(item),
	}
}

func isOpenAI2ToolOutputType(itemType string) bool {
	itemType = strings.ToLower(strings.TrimSpace(itemType))
	return itemType != "" && itemType != "message" && itemType != "reasoning"
}

func openAI2ToolItemInput(item map[string]interface{}) map[string]interface{} {
	for _, key := range []string{"arguments", "input"} {
		if raw := strings.TrimSpace(stringFromMap(item, key)); raw != "" {
			if parsed := parseJSONObjectString(raw); parsed != nil {
				return parsed
			}
			return map[string]interface{}{key: raw}
		}
	}
	if action, ok := item["action"].(map[string]interface{}); ok && len(action) > 0 {
		return action
	}
	return map[string]interface{}{}
}

func parseJSONObjectString(raw string) map[string]interface{} {
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil || parsed == nil {
		return nil
	}
	return parsed
}

func openAI2ToolItemInputDelta(item map[string]interface{}) string {
	input := openAI2ToolItemInput(item)
	if len(input) == 0 {
		return ""
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// ClaudeStreamToOpenAI2 converts Claude SSE event to OpenAI Responses stream event
func ClaudeStreamToOpenAI2(event []byte, ctx *transformer.StreamContext) ([]byte, error) {
	eventType, jsonData := parseSSE(event)
	if jsonData == "" {
		return nil, nil
	}

	var data map[string]interface{}
	if err := json.Unmarshal([]byte(jsonData), &data); err != nil {
		return nil, nil
	}

	// Check for error response
	if errType, ok := data["type"].(string); ok && errType == "error" {
		if errData, ok := data["error"].(map[string]interface{}); ok {
			if msg, ok := errData["message"].(string); ok {
				return nil, fmt.Errorf("upstream error: %s", msg)
			}
		}
	}

	var result strings.Builder
	writeEvent := func(evt map[string]interface{}) {
		d, _ := json.Marshal(evt)
		result.WriteString(fmt.Sprintf("data: %s\n\n", d))
	}

	switch eventType {
	case "message_start":
		if msg, ok := data["message"].(map[string]interface{}); ok {
			ctx.MessageID, _ = msg["id"].(string)
			if usage, ok := msg["usage"].(map[string]interface{}); ok {
				if in, ok := usage["input_tokens"].(float64); ok {
					ctx.InputTokens = int(in)
				}
			}
		}
		writeEvent(map[string]interface{}{
			"type": "response.created",
			"response": map[string]interface{}{
				"id": ctx.MessageID, "object": "response", "status": "in_progress",
			},
		})

	case "content_block_start":
		block, ok := data["content_block"].(map[string]interface{})
		if !ok {
			return nil, nil
		}
		idx, _ := data["index"].(float64)
		blockIdx := int(idx)

		switch block["type"] {
		case "text":
			ctx.ContentBlockStarted = true
			ctx.ContentIndex = blockIdx
			// output_item.added
			writeEvent(map[string]interface{}{
				"type": "response.output_item.added", "output_index": blockIdx,
				"item": map[string]interface{}{
					"type": "message", "id": fmt.Sprintf("msg_%s_%d", ctx.MessageID, blockIdx),
					"role": "assistant", "status": "in_progress", "content": []interface{}{},
				},
			})
			// content_part.added
			writeEvent(map[string]interface{}{
				"type": "response.content_part.added", "output_index": blockIdx, "content_index": 0,
				"part": map[string]interface{}{"type": "output_text", "text": ""},
			})
		case "tool_use":
			ctx.ToolBlockStarted = true
			ctx.ToolIndex = blockIdx
			ctx.CurrentToolID, _ = block["id"].(string)
			ctx.CurrentToolName, _ = block["name"].(string)
			// output_item.added for function_call
			writeEvent(map[string]interface{}{
				"type": "response.output_item.added", "output_index": blockIdx,
				"item": map[string]interface{}{
					"type": "function_call", "id": ctx.CurrentToolID,
					"call_id": ctx.CurrentToolID, "name": ctx.CurrentToolName,
					"arguments": "", "status": "in_progress",
				},
			})
		}

	case "content_block_delta":
		delta, ok := data["delta"].(map[string]interface{})
		if !ok {
			return nil, nil
		}
		switch delta["type"] {
		case "text_delta":
			writeEvent(map[string]interface{}{
				"type": "response.output_text.delta", "output_index": ctx.ContentIndex,
				"content_index": 0, "delta": delta["text"],
			})
		case "input_json_delta":
			partial := delta["partial_json"].(string)
			ctx.ToolArguments += partial
			writeEvent(map[string]interface{}{
				"type":         "response.function_call_arguments.delta",
				"output_index": ctx.ToolIndex, "delta": partial,
			})
		}

	case "content_block_stop":
		idx, _ := data["index"].(float64)
		blockIdx := int(idx)

		if ctx.ToolBlockStarted && blockIdx == ctx.ToolIndex {
			// function_call_arguments.done
			writeEvent(map[string]interface{}{
				"type":         "response.function_call_arguments.done",
				"output_index": blockIdx, "arguments": ctx.ToolArguments,
			})
			// output_item.done for function_call
			writeEvent(map[string]interface{}{
				"type": "response.output_item.done", "output_index": blockIdx,
				"item": map[string]interface{}{
					"type": "function_call", "id": ctx.CurrentToolID,
					"call_id": ctx.CurrentToolID, "name": ctx.CurrentToolName,
					"arguments": ctx.ToolArguments, "status": "completed",
				},
			})
			ctx.ToolBlockStarted = false
			ctx.ToolArguments = ""
		} else if ctx.ContentBlockStarted && blockIdx == ctx.ContentIndex {
			// output_text.done - need accumulated text, use empty for now
			writeEvent(map[string]interface{}{
				"type": "response.output_text.done", "output_index": blockIdx, "content_index": 0,
			})
			// content_part.done
			writeEvent(map[string]interface{}{
				"type": "response.content_part.done", "output_index": blockIdx, "content_index": 0,
				"part": map[string]interface{}{"type": "output_text"},
			})
			// output_item.done
			writeEvent(map[string]interface{}{
				"type": "response.output_item.done", "output_index": blockIdx,
				"item": map[string]interface{}{
					"type": "message", "id": fmt.Sprintf("msg_%s_%d", ctx.MessageID, blockIdx),
					"role": "assistant", "status": "completed",
				},
			})
			ctx.ContentBlockStarted = false
		}

	case "message_delta":
		if usage, ok := data["usage"].(map[string]interface{}); ok {
			if out, ok := usage["output_tokens"].(float64); ok {
				ctx.OutputTokens = int(out)
			}
		}

	case "message_stop":
		writeEvent(map[string]interface{}{
			"type": "response.completed",
			"response": map[string]interface{}{
				"id": ctx.MessageID, "object": "response", "status": "completed",
				"usage": map[string]interface{}{
					"input_tokens": ctx.InputTokens, "output_tokens": ctx.OutputTokens,
					"total_tokens": ctx.InputTokens + ctx.OutputTokens,
				},
			},
		})
		result.WriteString("data: [DONE]\n\n")
	}

	if result.Len() > 0 {
		return []byte(result.String()), nil
	}
	return nil, nil
}

// OpenAI2StreamToClaude converts OpenAI Responses stream event to Claude SSE event
func OpenAI2StreamToClaude(event []byte, ctx *transformer.StreamContext) ([]byte, error) {
	_, jsonData := parseSSE(event)
	if jsonData == "" || jsonData == "[DONE]" {
		if jsonData == "[DONE]" {
			var result []byte
			emitText, emitThinking := makeThinkEmitters(ctx, &result)
			flushThinkTaggedStream(ctx, emitText, emitThinking)
			if ctx.ThinkingBlockStarted {
				result = append(result, buildClaudeEvent("content_block_stop", map[string]interface{}{"index": ctx.ThinkingIndex})...)
				ctx.ThinkingBlockStarted = false
			}
			if ctx.ContentBlockStarted {
				result = append(result, buildClaudeEvent("content_block_stop", map[string]interface{}{"index": ctx.ContentIndex})...)
				ctx.ContentBlockStarted = false
			}
			if ctx.ToolBlockStarted {
				result = append(result, buildClaudeEvent("content_block_stop", map[string]interface{}{"index": ctx.ToolIndex})...)
				ctx.ToolBlockStarted = false
			}
			if !ctx.FinishReasonSent {
				result = append(result, buildClaudeEvent("message_stop", map[string]interface{}{})...)
				ctx.FinishReasonSent = true
			}
			return result, nil
		}
		return nil, nil
	}

	var evt transformer.OpenAI2StreamEvent
	if err := json.Unmarshal([]byte(jsonData), &evt); err != nil {
		return nil, nil
	}
	var rawEvent map[string]interface{}
	_ = json.Unmarshal([]byte(jsonData), &rawEvent)

	var result []byte

	switch evt.Type {
	case "response.created":
		if evt.Response != nil {
			ctx.MessageID = evt.Response.ID
			if evt.Response.Usage.InputTokens > 0 {
				ctx.InputTokens = evt.Response.Usage.InputTokens
			}
			if evt.Response.Usage.OutputTokens > 0 {
				ctx.OutputTokens = evt.Response.Usage.OutputTokens
			}
		}
		result = append(result, buildClaudeEvent("message_start", map[string]interface{}{
			"message": map[string]interface{}{
				"id": ctx.MessageID, "type": "message", "role": "assistant", "content": []interface{}{},
				"model": ctx.ModelName, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]interface{}{"input_tokens": ctx.InputTokens, "output_tokens": ctx.OutputTokens},
			},
		})...)

	case "response.output_text.delta":
		content := ctx.ThinkingBuffer + evt.Delta
		ctx.ThinkingBuffer = ""

		emitText, emitThinking := makeThinkEmitters(ctx, &result)
		emitTextWithClose := func(text string) {
			if text == "" {
				return
			}
			if ctx.ThinkingBlockStarted && !ctx.ContentBlockStarted && !ctx.InThinkingTag {
				result = append(result, buildClaudeEvent("content_block_stop", map[string]interface{}{"index": ctx.ThinkingIndex})...)
				ctx.ThinkingBlockStarted = false
			}
			emitText(text)
		}
		emitThinkingWithClose := func(text string) {
			if text == "" {
				return
			}
			emitThinking(text)
			if ctx.ThinkingBlockStarted {
				result = append(result, buildClaudeEvent("content_block_stop", map[string]interface{}{"index": ctx.ThinkingIndex})...)
				ctx.ThinkingBlockStarted = false
			}
		}

		consumeThinkTaggedStream(content, ctx, emitTextWithClose, emitThinkingWithClose)

	case "response.output_item.added":
		rawItem, _ := rawEvent["item"].(map[string]interface{})
		itemType := ""
		if rawItem != nil {
			itemType = strings.ToLower(strings.TrimSpace(stringFromMap(rawItem, "type")))
		} else if evt.Item != nil {
			itemType = strings.ToLower(strings.TrimSpace(evt.Item.Type))
		}
		if itemType == "function_call" || (itemType != "" && isOpenAI2ToolOutputType(itemType)) {
			if ctx.ThinkingBlockStarted {
				result = append(result, buildClaudeEvent("content_block_stop", map[string]interface{}{"index": ctx.ThinkingIndex})...)
				ctx.ThinkingBlockStarted = false
			}
			// Close text block if open
			if ctx.ContentBlockStarted {
				result = append(result, buildClaudeEvent("content_block_stop", map[string]interface{}{"index": ctx.ContentIndex})...)
				ctx.ContentBlockStarted = false
				ctx.ContentIndex++
			}
			ctx.ToolBlockStarted = true
			ctx.ToolIndex = ctx.ContentIndex
			if rawItem != nil {
				ctx.CurrentToolID = firstStringFromMap(rawItem, "call_id", "id")
				ctx.CurrentToolName = firstStringFromMap(rawItem, "name")
			} else if evt.Item != nil {
				ctx.CurrentToolID = evt.Item.CallID
				if ctx.CurrentToolID == "" {
					ctx.CurrentToolID = evt.Item.ID
				}
				ctx.CurrentToolName = evt.Item.Name
			}
			if ctx.CurrentToolID == "" {
				ctx.CurrentToolID = "call_" + strings.ReplaceAll(itemType, "-", "_")
			}
			if ctx.CurrentToolName == "" {
				ctx.CurrentToolName = itemType
			}
			ctx.ToolArguments = ""
			result = append(result, buildClaudeEvent("content_block_start", map[string]interface{}{
				"index": ctx.ToolIndex, "content_block": map[string]interface{}{
					"type": "tool_use", "id": ctx.CurrentToolID, "name": ctx.CurrentToolName, "input": map[string]interface{}{},
				},
			})...)
			if rawItem != nil {
				if inputDelta := openAI2ToolItemInputDelta(rawItem); inputDelta != "" && itemType != "function_call" {
					ctx.ToolArguments = inputDelta
					result = append(result, buildClaudeEvent("content_block_delta", map[string]interface{}{
						"index": ctx.ToolIndex, "delta": map[string]interface{}{"type": "input_json_delta", "partial_json": inputDelta},
					})...)
				}
			}
		}

	case "response.function_call_arguments.delta":
		if ctx.ToolBlockStarted {
			ctx.ToolArguments += evt.Delta
			result = append(result, buildClaudeEvent("content_block_delta", map[string]interface{}{
				"index": ctx.ToolIndex, "delta": map[string]interface{}{"type": "input_json_delta", "partial_json": evt.Delta},
			})...)
		}

	case "response.custom_tool_call_input.delta":
		if ctx.ToolBlockStarted {
			ctx.ToolArguments += evt.Delta
			result = append(result, buildClaudeEvent("content_block_delta", map[string]interface{}{
				"index": ctx.ToolIndex, "delta": map[string]interface{}{"type": "input_json_delta", "partial_json": evt.Delta},
			})...)
		}

	case "response.custom_tool_call_input.done":
		if ctx.ToolBlockStarted && ctx.ToolArguments == "" {
			if input := strings.TrimSpace(stringFromMap(rawEvent, "input")); input != "" {
				if parsed := parseJSONObjectString(input); parsed != nil {
					encoded, _ := json.Marshal(parsed)
					input = string(encoded)
				} else {
					encoded, _ := json.Marshal(map[string]interface{}{"input": input})
					input = string(encoded)
				}
				ctx.ToolArguments = input
				result = append(result, buildClaudeEvent("content_block_delta", map[string]interface{}{
					"index": ctx.ToolIndex, "delta": map[string]interface{}{"type": "input_json_delta", "partial_json": input},
				})...)
			}
		}

	case "response.output_item.done":
		rawItem, _ := rawEvent["item"].(map[string]interface{})
		itemType := ""
		if rawItem != nil {
			itemType = strings.ToLower(strings.TrimSpace(stringFromMap(rawItem, "type")))
		} else if evt.Item != nil {
			itemType = strings.ToLower(strings.TrimSpace(evt.Item.Type))
		}
		if ctx.ToolBlockStarted && (itemType == "function_call" || isOpenAI2ToolOutputType(itemType)) {
			if rawItem != nil && ctx.ToolArguments == "" {
				if inputDelta := openAI2ToolItemInputDelta(rawItem); inputDelta != "" {
					ctx.ToolArguments = inputDelta
					result = append(result, buildClaudeEvent("content_block_delta", map[string]interface{}{
						"index": ctx.ToolIndex, "delta": map[string]interface{}{"type": "input_json_delta", "partial_json": inputDelta},
					})...)
				}
			}
			result = append(result, buildClaudeEvent("content_block_stop", map[string]interface{}{"index": ctx.ToolIndex})...)
			ctx.ToolBlockStarted = false
			ctx.ContentIndex++
		}

	case "response.completed":
		if evt.Response != nil {
			if evt.Response.Usage.InputTokens > 0 {
				ctx.InputTokens = evt.Response.Usage.InputTokens
			}
			if evt.Response.Usage.OutputTokens > 0 {
				ctx.OutputTokens = evt.Response.Usage.OutputTokens
			}
		}
		emitText, emitThinking := makeThinkEmitters(ctx, &result)
		flushThinkTaggedStream(ctx, emitText, emitThinking)
		if ctx.ThinkingBlockStarted {
			result = append(result, buildClaudeEvent("content_block_stop", map[string]interface{}{"index": ctx.ThinkingIndex})...)
			ctx.ThinkingBlockStarted = false
		}
		if ctx.ContentBlockStarted {
			result = append(result, buildClaudeEvent("content_block_stop", map[string]interface{}{"index": ctx.ContentIndex})...)
			ctx.ContentBlockStarted = false
		}
		stopReason := "end_turn"
		if ctx.ToolIndex > 0 || ctx.CurrentToolID != "" {
			stopReason = "tool_use"
		}
		result = append(result, buildClaudeEvent("message_delta", map[string]interface{}{
			"delta": map[string]interface{}{"stop_reason": stopReason, "stop_sequence": nil},
			"usage": map[string]interface{}{"output_tokens": ctx.OutputTokens},
		})...)
		result = append(result, buildClaudeEvent("message_stop", map[string]interface{}{})...)
		ctx.FinishReasonSent = true
	}

	return result, nil
}

// Helper functions

func convertClaudeMessageToOpenAI2Items(content []interface{}, role string) []map[string]interface{} {
	var items []map[string]interface{}
	var messageParts []map[string]interface{}
	textType := "input_text"
	if role == "assistant" {
		textType = "output_text"
	}

	flushMessage := func() {
		if len(messageParts) == 0 {
			return
		}
		items = append(items, map[string]interface{}{
			"type":    "message",
			"role":    role,
			"content": messageParts,
		})
		messageParts = nil
	}

	for _, block := range content {
		m, ok := block.(map[string]interface{})
		if !ok {
			continue
		}

		blockType, _ := m["type"].(string)
		switch blockType {
		case "text":
			text, _ := m["text"].(string)
			messageParts = append(messageParts, map[string]interface{}{"type": textType, "text": text})
		case "thinking":
			// Skip thinking blocks - they are Claude's internal reasoning
			continue
		case "tool_use":
			flushMessage()
			callID, _ := m["id"].(string)
			name, _ := m["name"].(string)
			args, _ := json.Marshal(m["input"])
			items = append(items, map[string]interface{}{
				"type":      "function_call",
				"call_id":   callID,
				"name":      name,
				"arguments": string(args),
			})
		case "tool_result":
			flushMessage()
			callID, _ := m["tool_use_id"].(string)
			items = append(items, map[string]interface{}{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  toolResultToString(m["content"]),
			})
		}
	}
	flushMessage()

	return items
}

func toolResultToString(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(data)
	}
}

func convertOpenAI2InputToClaude(input interface{}) []map[string]interface{} {
	var messages []map[string]interface{}

	switch v := input.(type) {
	case string:
		messages = append(messages, map[string]interface{}{"role": "user", "content": v})
	case []interface{}:
		var pendingToolUses []map[string]interface{}
		var pendingToolResults []map[string]interface{}

		for _, item := range v {
			itemMap, ok := item.(map[string]interface{})
			if !ok {
				continue
			}

			itemType, _ := itemMap["type"].(string)
			switch itemType {
			case "message":
				// Flush pending tool uses before user message
				if len(pendingToolUses) > 0 {
					messages = append(messages, map[string]interface{}{"role": "assistant", "content": pendingToolUses})
					pendingToolUses = nil
				}
				// Flush pending tool results before user message
				if len(pendingToolResults) > 0 {
					messages = append(messages, map[string]interface{}{"role": "user", "content": pendingToolResults})
					pendingToolResults = nil
				}

				role, _ := itemMap["role"].(string)
				content := convertOpenAI2ContentToClaude(itemMap["content"], role)
				messages = append(messages, map[string]interface{}{"role": role, "content": content})

			case "function_call":
				// Convert to Claude tool_use
				callID, _ := itemMap["call_id"].(string)
				if callID == "" {
					callID, _ = itemMap["id"].(string)
				}
				name, _ := itemMap["name"].(string)
				argsStr, _ := itemMap["arguments"].(string)
				var args interface{}
				if err := json.Unmarshal([]byte(argsStr), &args); err != nil {
					args = map[string]interface{}{}
				}
				pendingToolUses = append(pendingToolUses, map[string]interface{}{
					"type": "tool_use", "id": callID, "name": name, "input": args,
				})

			case "function_call_output":
				// Flush pending tool uses first
				if len(pendingToolUses) > 0 {
					messages = append(messages, map[string]interface{}{"role": "assistant", "content": pendingToolUses})
					pendingToolUses = nil
				}
				// Convert to Claude tool_result
				callID, _ := itemMap["call_id"].(string)
				output, _ := itemMap["output"].(string)
				pendingToolResults = append(pendingToolResults, map[string]interface{}{
					"type": "tool_result", "tool_use_id": callID, "content": output,
				})
			}
		}

		// Flush remaining
		if len(pendingToolUses) > 0 {
			messages = append(messages, map[string]interface{}{"role": "assistant", "content": pendingToolUses})
		}
		if len(pendingToolResults) > 0 {
			messages = append(messages, map[string]interface{}{"role": "user", "content": pendingToolResults})
		}
	}
	return messages
}

func convertOpenAI2ContentToClaude(content interface{}, role string) interface{} {
	arr, ok := content.([]interface{})
	if !ok {
		return content
	}

	var result []map[string]interface{}
	for _, part := range arr {
		partMap, ok := part.(map[string]interface{})
		if !ok {
			continue
		}
		switch partMap["type"] {
		case "input_text", "output_text":
			result = append(result, map[string]interface{}{"type": "text", "text": partMap["text"]})
		}
	}

	if len(result) == 1 {
		if text, ok := result[0]["text"].(string); ok {
			return text
		}
	}
	return result
}
