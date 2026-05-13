package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/logger"
	"github.com/lich0821/ccNexus/internal/tokencount"
	"github.com/lich0821/ccNexus/internal/transformer"
)

const (
	streamFinishCompleted             = "completed"
	streamFinishClientCanceled        = "client_canceled"
	streamFinishUpstreamStreamError   = "upstream_stream_error"
	streamFinishDownstreamWriteFailed = "downstream_write_failed"
	streamFinishTransformFailed       = "transform_failed"
)

type downstreamStreamSession struct {
	w                 http.ResponseWriter
	flusher           http.Flusher
	heartbeatInterval time.Duration
	done              chan struct{}
	mu                sync.Mutex
	closeOnce         sync.Once
	heartbeatOnce     sync.Once
	started           bool
	closed            bool
}

func newDownstreamStreamSession(w http.ResponseWriter, heartbeatInterval time.Duration) *downstreamStreamSession {
	flusher, _ := w.(http.Flusher)
	return &downstreamStreamSession{
		w:                 w,
		flusher:           flusher,
		heartbeatInterval: heartbeatInterval,
		done:              make(chan struct{}),
	}
}

func (s *downstreamStreamSession) Start() error {
	if s == nil {
		return nil
	}
	if s.flusher == nil {
		return fmt.Errorf("response writer does not support flushing")
	}

	shouldStartHeartbeat := false
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("downstream stream is closed")
	}
	if !s.started {
		header := s.w.Header()
		header.Set("Content-Type", "text/event-stream; charset=utf-8")
		header.Set("Cache-Control", "no-cache")
		header.Set("X-Accel-Buffering", "no")
		s.w.WriteHeader(http.StatusOK)
		if _, err := s.w.Write([]byte(": ccnexus waiting for upstream\n\n")); err != nil {
			s.mu.Unlock()
			return err
		}
		s.flusher.Flush()
		s.started = true
		shouldStartHeartbeat = s.heartbeatInterval > 0
	}
	s.mu.Unlock()

	if shouldStartHeartbeat {
		s.heartbeatOnce.Do(func() {
			go s.heartbeatLoop()
		})
	}
	return nil
}

func (s *downstreamStreamSession) Started() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

func (s *downstreamStreamSession) Write(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if err := s.Start(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("downstream stream is closed")
	}
	if _, err := s.w.Write(data); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

func (s *downstreamStreamSession) WriteError(message string) error {
	if strings.TrimSpace(message) == "" {
		message = "stream failed"
	}
	payload, err := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{
			"type":    "service_unavailable",
			"message": message,
		},
	})
	if err != nil {
		return err
	}
	return s.Write([]byte(fmt.Sprintf("event: error\ndata: %s\n\n", payload)))
}

func writeDownstreamStreamFailure(streamSession *downstreamStreamSession, clientFormat ClientFormat, transformerName string, code string, message string) error {
	if streamSession == nil {
		return nil
	}
	if shouldWriteResponsesStreamFailure(clientFormat, transformerName) {
		return streamSession.Write(buildResponsesStreamFailureEvent(code, message))
	}
	return streamSession.WriteError(message)
}

func shouldWriteResponsesStreamFailure(clientFormat ClientFormat, transformerName string) bool {
	return clientFormat == ClientFormatOpenAIResponses || shouldEnsureResponsesStreamCompletion(transformerName)
}

func buildResponsesStreamFailureEvent(code string, message string) []byte {
	code = sanitizeLogField(code)
	if code == "" {
		code = "upstream_stream_error"
	}
	if strings.TrimSpace(message) == "" {
		message = "stream failed"
	}
	payload := map[string]interface{}{
		"type": "response.failed",
		"response": map[string]interface{}{
			"id":         "resp_ccnexus_failed",
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "failed",
			"error": map[string]interface{}{
				"code":    code,
				"message": message,
			},
			"output": []interface{}{},
			"usage":  nil,
		},
	}
	encoded, _ := json.Marshal(payload)
	return []byte(fmt.Sprintf("event: response.failed\ndata: %s\n\ndata: [DONE]\n\n", encoded))
}

func (s *downstreamStreamSession) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		close(s.done)
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
	})
}

func (s *downstreamStreamSession) heartbeatLoop() {
	ticker := time.NewTicker(s.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_ = s.writeComment("ccnexus waiting for upstream")
		case <-s.done:
			return
		}
	}
}

func (s *downstreamStreamSession) writeComment(message string) error {
	if err := s.Start(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("downstream stream is closed")
	}
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", message); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

type streamResponseResult struct {
	InputTokens       int
	OutputTokens      int
	OutputText        string
	Completed         bool
	WroteData         bool
	WroteSemanticData bool
	Reason            string
	Err               error
}

// handleStreamingResponse processes streaming SSE responses
func (p *Proxy) handleStreamingResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response, endpoint config.Endpoint, trans transformer.Transformer, transformerName string, thinkingEnabled bool, modelName string, bodyBytes []byte, credentialID int64, streamSession *downstreamStreamSession) streamResponseResult {
	result := streamResponseResult{}

	flusher, ok := w.(http.Flusher)
	if streamSession == nil && !ok {
		logger.Error("[%s] ResponseWriter does not support flushing", endpoint.Name)
		resp.Body.Close()
		result.Reason = streamFinishDownstreamWriteFailed
		result.Err = fmt.Errorf("response writer does not support flushing")
		return result
	}

	headersCommitted := false
	commitHeaders := func() {
		if streamSession != nil {
			headersCommitted = true
			return
		}
		if headersCommitted {
			return
		}
		for key, values := range resp.Header {
			if key == "Content-Length" || key == "Content-Encoding" {
				continue
			}
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		if strings.TrimSpace(w.Header().Get("Content-Type")) == "" {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		}
		w.WriteHeader(resp.StatusCode)
		headersCommitted = true
	}
	writeData := func(data []byte) error {
		if streamSession != nil {
			return streamSession.Write(data)
		}
		commitHeaders()
		if _, writeErr := w.Write(data); writeErr != nil {
			return writeErr
		}
		flusher.Flush()
		return nil
	}

	// Handle gzip-encoded response body
	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gzipReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			logger.Error("[%s] Failed to create gzip reader: %v", endpoint.Name, err)
			resp.Body.Close()
			result.Reason = streamFinishUpstreamStreamError
			result.Err = err
			return result
		}
		defer gzipReader.Close()
		reader = gzipReader
	}

	// Create stream context for all transformers except pure passthrough
	var streamCtx *transformer.StreamContext
	switch transformerName {
	case "cx_chat_openai", "cx_resp_openai2":
		// Pure passthrough - no context needed
	default:
		// cc_claude needs context for input_tokens fallback
		streamCtx = transformer.NewStreamContext()
		streamCtx.ModelName = modelName
		// Pre-estimate input tokens for fallback
		if bodyBytes != nil {
			streamCtx.InputTokens = p.estimateInputTokens(bodyBytes)
		}
	}

	scanner := bufio.NewScanner(reader)
	// Increase buffer sizes to handle large SSE events (e.g., large file reads in tool calls)
	buf := make([]byte, 0, 128*1024) // 128KB initial buffer (was 64KB)
	scanner.Buffer(buf, 2*1024*1024) // 2MB max buffer (was 1MB)

	var inputTokens, outputTokens int
	var buffer bytes.Buffer
	var pendingWrites bytes.Buffer
	var outputText strings.Builder
	eventCount := 0
	streamDone := false
	sawDoneMarker := false
	responseCompletedSeen := false
	responseID := ""
	semanticDataSeen := false
	emptyKind := ""

	writeTransformedEvent := func(data []byte, semantic bool) error {
		if len(data) == 0 {
			return nil
		}
		if !semanticDataSeen {
			pendingWrites.Write(data)
			if !semantic {
				return nil
			}
			commitHeaders()
			if writeErr := writeData(pendingWrites.Bytes()); writeErr != nil {
				return writeErr
			}
			pendingWrites.Reset()
			semanticDataSeen = true
			result.WroteSemanticData = true
			result.WroteData = true
			return nil
		}

		if writeErr := writeData(data); writeErr != nil {
			return writeErr
		}
		result.WroteData = true
		return nil
	}

	writeSyntheticResponsesCompletion := func(includeDone bool) error {
		if !shouldEnsureResponsesStreamCompletion(transformerName) || responseCompletedSeen {
			return nil
		}
		if inputTokens == 0 && bodyBytes != nil {
			inputTokens = p.estimateInputTokens(bodyBytes)
			if streamCtx != nil {
				streamCtx.InputTokens = inputTokens
			}
		}
		if outputTokens == 0 && outputText.Len() > 0 {
			outputTokens = tokencount.EstimateOutputTokens(outputText.String())
			if streamCtx != nil {
				streamCtx.OutputTokens = outputTokens
			}
		}
		completionEvent := buildSyntheticResponsesCompletionEvent(responseID, inputTokens, outputTokens, outputText.String())
		streamInspection := inspectSemanticStreamEvent(completionEvent)
		if writeErr := writeTransformedEvent(completionEvent, streamInspection.HasOutput || result.WroteSemanticData); writeErr != nil {
			return writeErr
		}
		responseCompletedSeen = true
		if result.Err == nil {
			result.Completed = true
			result.Reason = streamFinishCompleted
		}
		if includeDone {
			if writeErr := writeTransformedEvent([]byte("data: [DONE]\n\n"), false); writeErr != nil {
				return writeErr
			}
		}
		return nil
	}

	for scanner.Scan() && !streamDone {
		line := scanner.Text()

		if strings.Contains(line, "data: [DONE]") {
			streamDone = true
			sawDoneMarker = true
			result.Completed = true
			result.Reason = streamFinishCompleted

			// Token Usage Fallback: Inject message_delta with estimated output_tokens before [DONE]
			if outputTokens == 0 && outputText.Len() > 0 {
				outputTokens = tokencount.EstimateOutputTokens(outputText.String())
				logger.Debug("[%s] Token fallback before [DONE]: estimated output_tokens=%d", endpoint.Name, outputTokens)

				// Update stream context for transformer fallback
				if streamCtx != nil {
					streamCtx.OutputTokens = outputTokens
				}

				if shouldInjectClaudeMessageDeltaUsage(transformerName) {
					// Inject message_delta event with usage
					deltaEvent := fmt.Sprintf("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":%d}}\n\n", outputTokens)
					if writeErr := writeTransformedEvent([]byte(deltaEvent), false); writeErr != nil {
						result.Completed = false
						result.Reason = streamFinishDownstreamWriteFailed
						result.Err = writeErr
						break
					}
				}
			}

			if result.WroteSemanticData && !responseCompletedSeen {
				if writeErr := writeSyntheticResponsesCompletion(false); writeErr != nil {
					result.Completed = false
					result.Reason = streamFinishDownstreamWriteFailed
					result.Err = writeErr
					break
				}
			}

			buffer.WriteString(line + "\n")
			eventData := buffer.Bytes()
			logger.DebugLog("[%s] SSE Event #%d (Original): %s", endpoint.Name, eventCount+1, string(eventData))

			transformedEvent, err := p.transformStreamEvent(eventData, trans, transformerName, streamCtx)
			if err == nil && len(transformedEvent) > 0 {
				logger.DebugLog("[%s] SSE Event #%d (Transformed): %s", endpoint.Name, eventCount+1, string(transformedEvent))
				updateResponsesStreamCompletionState(transformedEvent, &responseID, &responseCompletedSeen)
				streamInspection := inspectSemanticStreamEvent(transformedEvent)
				if streamInspection.EmptyKind != "" {
					emptyKind = streamInspection.EmptyKind
				}
				if writeErr := writeTransformedEvent(transformedEvent, streamInspection.HasOutput); writeErr != nil {
					result.Completed = false
					result.Reason = streamFinishDownstreamWriteFailed
					result.Err = writeErr
					break
				}
			} else if err != nil {
				result.Completed = false
				result.Reason = streamFinishTransformFailed
				result.Err = err
			}
			break
		}

		buffer.WriteString(line + "\n")

		if line == "" {
			eventCount++
			eventData := buffer.Bytes()
			logger.DebugLog("[%s] SSE Event #%d (Original): %s", endpoint.Name, eventCount, string(eventData))

			p.captureCodexRateLimitsFromEvent(endpoint, credentialID, eventData)

			// Extract usage from original upstream events first. Some transformers may
			// not preserve usage fields in transformed events.
			p.extractTokensFromEvent(eventData, &inputTokens, &outputTokens)

			// Check if this is a message_stop event (Token Usage Fallback)
			isMessageStop := p.isMessageStopEvent(eventData)
			if isMessageStop && outputTokens == 0 && outputText.Len() > 0 {
				outputTokens = tokencount.EstimateOutputTokens(outputText.String())
				logger.Debug("[%s] Token fallback before message_stop: estimated output_tokens=%d", endpoint.Name, outputTokens)

				// Update stream context for transformer fallback
				if streamCtx != nil {
					streamCtx.OutputTokens = outputTokens
				}

				if shouldInjectClaudeMessageDeltaUsage(transformerName) {
					// Inject message_delta event with usage before message_stop
					deltaEvent := fmt.Sprintf("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":%d}}\n\n", outputTokens)
					if writeErr := writeTransformedEvent([]byte(deltaEvent), false); writeErr != nil {
						result.Reason = streamFinishDownstreamWriteFailed
						result.Err = writeErr
						streamDone = true
						break
					}
				}
			}

			transformedEvent, err := p.transformStreamEvent(eventData, trans, transformerName, streamCtx)
			if err != nil {
				logger.Error("[%s] Failed to transform SSE event: %v", endpoint.Name, err)
				result.Reason = streamFinishTransformFailed
				result.Err = err
				streamDone = true
			} else if len(transformedEvent) > 0 {
				logger.DebugLog("[%s] SSE Event #%d (Transformed): %s", endpoint.Name, eventCount, string(transformedEvent))

				p.extractTokensFromEvent(transformedEvent, &inputTokens, &outputTokens)
				p.extractTextFromEvent(transformedEvent, &outputText)
				updateResponsesStreamCompletionState(transformedEvent, &responseID, &responseCompletedSeen)
				streamInspection := inspectSemanticStreamEvent(transformedEvent)
				semanticEvent := streamInspection.HasOutput
				if streamInspection.EmptyKind != "" {
					emptyKind = streamInspection.EmptyKind
				}

				if writeErr := writeTransformedEvent(transformedEvent, semanticEvent); writeErr != nil {
					// Client disconnected (broken pipe) is normal for cancelled requests
					if strings.Contains(writeErr.Error(), "broken pipe") || strings.Contains(writeErr.Error(), "connection reset") {
						logger.Debug("[%s] Client disconnected: %v", endpoint.Name, writeErr)
						result.Reason = streamFinishClientCanceled
					} else {
						logger.Error("[%s] Failed to write transformed event: %v", endpoint.Name, writeErr)
						result.Reason = streamFinishDownstreamWriteFailed
					}
					result.Err = writeErr
					streamDone = true
					break
				}
			}
			buffer.Reset()
		}
	}

	if scanner.Err() == nil && !streamDone && buffer.Len() > 0 && result.Err == nil {
		eventCount++
		eventData := buffer.Bytes()
		logger.DebugLog("[%s] SSE Event #%d (Original EOF): %s", endpoint.Name, eventCount, string(eventData))

		p.captureCodexRateLimitsFromEvent(endpoint, credentialID, eventData)
		p.extractTokensFromEvent(eventData, &inputTokens, &outputTokens)

		isMessageStop := p.isMessageStopEvent(eventData)
		if isMessageStop && outputTokens == 0 && outputText.Len() > 0 {
			outputTokens = tokencount.EstimateOutputTokens(outputText.String())
			logger.Debug("[%s] Token fallback before EOF message_stop: estimated output_tokens=%d", endpoint.Name, outputTokens)
			if streamCtx != nil {
				streamCtx.OutputTokens = outputTokens
			}
			if shouldInjectClaudeMessageDeltaUsage(transformerName) {
				deltaEvent := fmt.Sprintf("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":%d}}\n\n", outputTokens)
				if writeErr := writeTransformedEvent([]byte(deltaEvent), false); writeErr != nil {
					result.Reason = streamFinishDownstreamWriteFailed
					result.Err = writeErr
				}
			}
		}

		if result.Err == nil {
			transformedEvent, err := p.transformStreamEvent(eventData, trans, transformerName, streamCtx)
			if err != nil {
				logger.Error("[%s] Failed to transform EOF SSE event: %v", endpoint.Name, err)
				result.Reason = streamFinishTransformFailed
				result.Err = err
			} else if len(transformedEvent) > 0 {
				logger.DebugLog("[%s] SSE Event #%d (Transformed EOF): %s", endpoint.Name, eventCount, string(transformedEvent))
				p.extractTokensFromEvent(transformedEvent, &inputTokens, &outputTokens)
				p.extractTextFromEvent(transformedEvent, &outputText)
				updateResponsesStreamCompletionState(transformedEvent, &responseID, &responseCompletedSeen)
				streamInspection := inspectSemanticStreamEvent(transformedEvent)
				if streamInspection.EmptyKind != "" {
					emptyKind = streamInspection.EmptyKind
				}
				if writeErr := writeTransformedEvent(transformedEvent, streamInspection.HasOutput); writeErr != nil {
					if strings.Contains(writeErr.Error(), "broken pipe") || strings.Contains(writeErr.Error(), "connection reset") {
						logger.Debug("[%s] Client disconnected: %v", endpoint.Name, writeErr)
						result.Reason = streamFinishClientCanceled
					} else {
						logger.Error("[%s] Failed to write transformed EOF event: %v", endpoint.Name, writeErr)
						result.Reason = streamFinishDownstreamWriteFailed
					}
					result.Err = writeErr
				}
			}
		}
		buffer.Reset()
	}

	scannerErr := scanner.Err()
	if scannerErr != nil {
		err := scannerErr
		errMsg := err.Error()
		result.Err = err
		if isClientCanceled(ctx, err) {
			result.Reason = streamFinishClientCanceled
			logger.Debug("[%s] Streaming canceled by client: %v", endpoint.Name, err)
		} else if strings.Contains(errMsg, "stream error") || strings.Contains(errMsg, "INTERNAL_ERROR") {
			result.Reason = streamFinishUpstreamStreamError
			requestSize := len(bodyBytes)
			sizeStr := formatRequestSize(requestSize)
			logger.Error("[%s] HTTP/2 stream error (Request size: %s / %d bytes): %v",
				endpoint.Name, sizeStr, requestSize, err)

			// Provide context based on request size
			if requestSize > 100*1024 { // > 100KB
				logger.Warn("[%s] Large request detected (%s). Consider: 1) Reading fewer files at once, 2) Using smaller code sections, 3) Breaking task into smaller requests",
					endpoint.Name, sizeStr)
			} else {
				logger.Warn("[%s] This error may occur due to upstream server limitations or network issues.", endpoint.Name)
			}
		} else {
			result.Reason = streamFinishUpstreamStreamError
			logger.Error("[%s] Scanner error: %v", endpoint.Name, err)
		}
	}
	if scannerErr == nil &&
		shouldEnsureResponsesStreamCompletion(transformerName) &&
		result.WroteSemanticData &&
		!responseCompletedSeen &&
		!sawDoneMarker &&
		result.Err == nil {
		if writeErr := writeSyntheticResponsesCompletion(true); writeErr != nil {
			result.Reason = streamFinishDownstreamWriteFailed
			result.Err = writeErr
		} else {
			result.Completed = false
			result.Reason = streamFinishUpstreamStreamError
			result.Err = fmt.Errorf("stream closed before response.completed")
			logger.Warn("[%s] Upstream stream closed before response.completed; sent synthetic completion and marked endpoint failed", endpoint.Name)
		}
	}
	if scannerErr == nil &&
		shouldEnsureResponsesStreamCompletion(transformerName) &&
		responseCompletedSeen &&
		!sawDoneMarker &&
		result.Err == nil {
		if writeErr := writeTransformedEvent([]byte("data: [DONE]\n\n"), false); writeErr != nil {
			result.Reason = streamFinishDownstreamWriteFailed
			result.Err = writeErr
		} else {
			result.Completed = true
			result.Reason = streamFinishCompleted
		}
	}
	if scannerErr != nil &&
		shouldEnsureResponsesStreamCompletion(transformerName) &&
		result.WroteSemanticData &&
		!responseCompletedSeen {
		if writeErr := writeSyntheticResponsesCompletion(true); writeErr != nil {
			logger.Debug("[%s] Failed to write synthetic completion after stream error: %v", endpoint.Name, writeErr)
		}
	}

	resp.Body.Close()
	if result.Reason == "" {
		result.Reason = streamFinishCompleted
		result.Completed = true
	}
	result.InputTokens = inputTokens
	result.OutputTokens = outputTokens
	result.OutputText = outputText.String()
	if result.Err == nil && result.Completed && !result.WroteSemanticData {
		result.Reason = retryReasonSemanticEmptyResponse
		result.Completed = false
		result.Err = newSemanticEmptyResponseError(emptyKind, outputTokens, outputText.Len())
	}
	return result
}

// handleStreamingAsNonStreaming aggregates SSE and returns a single non-stream response.
// This is used for Codex endpoints that require stream=true upstream while client requested non-stream.
func (p *Proxy) handleStreamingAsNonStreaming(w http.ResponseWriter, resp *http.Response, endpoint config.Endpoint, trans transformer.Transformer, credentialID int64) (int, int, string, error) {
	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gzipReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			resp.Body.Close()
			return 0, 0, "", err
		}
		defer gzipReader.Close()
		reader = gzipReader
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(reader)
	buf := make([]byte, 0, 128*1024)
	scanner.Buffer(buf, 2*1024*1024)

	var completedPayload []byte
	var lastJSONPayload []byte
	chatAccumulator := newOpenAIChatStreamAccumulator()
	responsesAccumulator := newOpenAIResponsesStreamAccumulator()
	sseEventType := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			sseEventType = ""
			continue
		}
		if parsedEventType := sseEventTypeFromLine(line); parsedEventType != "" {
			sseEventType = parsedEventType
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		jsonData := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if jsonData == "" || jsonData == "[DONE]" {
			continue
		}
		p.captureCodexRateLimitsFromEvent(endpoint, credentialID, []byte("data: "+jsonData+"\n\n"))

		var event map[string]interface{}
		if err := json.Unmarshal([]byte(jsonData), &event); err != nil {
			lastJSONPayload = []byte(jsonData)
			continue
		}
		ensureSSEEventType(event, sseEventType)
		if normalizedPayload, err := json.Marshal(event); err == nil {
			lastJSONPayload = normalizedPayload
		} else {
			lastJSONPayload = []byte(jsonData)
		}
		responsesAccumulator.addEvent(event)
		if chatAccumulator.addChunk(event) {
			continue
		}
		if eventType, _ := event["type"].(string); eventType != "response.completed" {
			continue
		}

		if responseObj, ok := event["response"]; ok {
			payload, err := json.Marshal(responseObj)
			if err != nil {
				return 0, 0, "", err
			}
			completedPayload = payload
		} else {
			completedPayload = lastJSONPayload
		}
		break
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, "", err
	}
	if len(completedPayload) == 0 {
		if chatAccumulator.hasData() {
			payload, err := chatAccumulator.payload()
			if err != nil {
				return 0, 0, "", err
			}
			completedPayload = payload
		}
	}
	if len(completedPayload) == 0 {
		if payload, ok := responsesAccumulator.payload(); ok {
			completedPayload = payload
		}
	}
	if len(completedPayload) == 0 {
		if len(lastJSONPayload) == 0 {
			return 0, 0, "", fmt.Errorf("stream closed before response.completed")
		}
		// Fallback for providers that don't emit type=response.completed but still
		// provide final JSON payload in the stream.
		completedPayload = lastJSONPayload
	}
	completedPayload = responsesAccumulator.patchCompletedPayload(completedPayload)

	transformedResp, err := trans.TransformResponse(completedPayload, false)
	if err != nil {
		return 0, 0, "", err
	}

	inputTokens, outputTokens := extractTokenUsage(transformedResp)
	transformedInputTokens, transformedOutputTokens := inputTokens, outputTokens
	upstreamInputTokens, upstreamOutputTokens := extractTokenUsage(completedPayload)
	if inputTokens == 0 && upstreamInputTokens > 0 {
		inputTokens = upstreamInputTokens
	}
	if outputTokens == 0 && upstreamOutputTokens > 0 {
		outputTokens = upstreamOutputTokens
	}
	outputText := extractResponseOutputText(transformedResp)

	logger.Debug(
		"[%s] Aggregated usage transformed(in=%d,out=%d) upstream(in=%d,out=%d) outputTextLen=%d",
		endpoint.Name,
		transformedInputTokens, transformedOutputTokens,
		upstreamInputTokens, upstreamOutputTokens,
		len(outputText),
	)

	if semanticErr := semanticEmptyErrorForResponse(transformedResp, outputTokens); semanticErr != nil {
		semanticErr.OutputTextLen = len(outputText)
		return 0, 0, "", semanticErr
	}

	for key, values := range resp.Header {
		if key == "Content-Length" || key == "Content-Encoding" || key == "Content-Type" {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(transformedResp)

	return inputTokens, outputTokens, outputText, nil
}

type openAIChatStreamAccumulator struct {
	id                string
	created           interface{}
	model             string
	systemFingerprint interface{}
	usage             map[string]interface{}
	choices           map[int]*openAIChatStreamChoice
	seen              bool
}

type openAIChatStreamChoice struct {
	index            int
	role             string
	content          string
	reasoningContent string
	toolCalls        map[int]*openAIChatStreamToolCall
	finishReason     interface{}
}

type openAIChatStreamToolCall struct {
	index     int
	id        string
	callType  string
	name      string
	arguments string
}

type openAIResponsesStreamAccumulator struct {
	deltaText     string
	doneText      string
	itemText      string
	responseID    string
	usage         map[string]interface{}
	functionCalls map[int]*openAIResponsesFunctionCall
	functionOrder []int
	genericItems  map[int]map[string]interface{}
	outputOrder   []int
}

type openAIResponsesFunctionCall struct {
	index     int
	id        string
	callID    string
	name      string
	arguments string
	status    string
}

func newOpenAIResponsesStreamAccumulator() *openAIResponsesStreamAccumulator {
	return &openAIResponsesStreamAccumulator{
		functionCalls: make(map[int]*openAIResponsesFunctionCall),
		genericItems:  make(map[int]map[string]interface{}),
	}
}

func (a *openAIResponsesStreamAccumulator) addEvent(event map[string]interface{}) {
	if a == nil || event == nil {
		return
	}
	eventType, _ := event["type"].(string)
	if response, ok := event["response"].(map[string]interface{}); ok {
		if id, ok := response["id"].(string); ok && id != "" && a.responseID == "" {
			a.responseID = id
		}
		if usage, ok := response["usage"].(map[string]interface{}); ok && len(usage) > 0 {
			a.usage = usage
		}
	}
	switch eventType {
	case "response.created":
		if response, ok := event["response"].(map[string]interface{}); ok {
			if id, ok := response["id"].(string); ok && id != "" && a.responseID == "" {
				a.responseID = id
			}
		}
	case "response.output_text.delta":
		if delta, ok := event["delta"].(string); ok {
			a.deltaText += delta
		}
	case "response.output_text.done":
		if a.deltaText == "" {
			if text, ok := event["text"].(string); ok {
				a.doneText += text
			}
		}
	case "response.content_part.done":
		if a.deltaText == "" && a.doneText == "" {
			if part, ok := event["part"].(map[string]interface{}); ok {
				a.itemText += responseContentPartText(part)
			}
		}
	case "response.output_item.done":
		if item, ok := event["item"].(map[string]interface{}); ok {
			switch itemType := responseOutputItemType(item); {
			case itemType == "function_call":
				a.addFunctionCallItem(event, item)
				return
			case isResponsesGenericOutputItemType(itemType):
				a.addOutputItem(event, item)
				return
			}
		}
		if a.deltaText == "" && a.doneText == "" {
			if item, ok := event["item"].(map[string]interface{}); ok {
				a.itemText += responseOutputItemText(item)
			}
		}
	case "response.output_item.added":
		if item, ok := event["item"].(map[string]interface{}); ok {
			switch itemType := responseOutputItemType(item); {
			case itemType == "function_call":
				a.addFunctionCallItem(event, item)
			case isResponsesGenericOutputItemType(itemType):
				a.addOutputItem(event, item)
			}
		}
	case "response.function_call_arguments.delta":
		if delta, ok := event["delta"].(string); ok && delta != "" {
			a.addFunctionCallArguments(event, delta, false)
		}
	case "response.function_call_arguments.done":
		if arguments, ok := event["arguments"].(string); ok {
			a.addFunctionCallArguments(event, arguments, true)
		}
	case "response.custom_tool_call_input.delta":
		if delta, ok := event["delta"].(string); ok && delta != "" {
			a.addOutputItemInput(event, delta, false)
		}
	case "response.custom_tool_call_input.done":
		if input, ok := event["input"].(string); ok {
			a.addOutputItemInput(event, input, true)
		}
	}
}

func (a *openAIResponsesStreamAccumulator) text() string {
	if a == nil {
		return ""
	}
	if a.deltaText != "" {
		return a.deltaText
	}
	if a.doneText != "" {
		return a.doneText
	}
	return a.itemText
}

func (a *openAIResponsesStreamAccumulator) payload() ([]byte, bool) {
	output := a.outputItems()
	if a == nil || len(output) == 0 {
		return nil, false
	}

	responseID := strings.TrimSpace(a.responseID)
	if responseID == "" {
		responseID = "resp_ccnexus_synthetic"
	}
	usage := a.usage
	if usage == nil {
		usage = map[string]interface{}{
			"input_tokens":  0,
			"output_tokens": 0,
			"total_tokens":  0,
		}
	}
	payload := map[string]interface{}{
		"id":     responseID,
		"object": "response",
		"status": "completed",
		"usage":  usage,
		"output": output,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

func (a *openAIResponsesStreamAccumulator) patchCompletedPayload(payload []byte) []byte {
	outputItems := a.outputItems()
	if len(outputItems) == 0 || len(payload) == 0 {
		return payload
	}
	if inspection := inspectSemanticResponse(payload); inspection.Recognized && inspection.HasOutput {
		return payload
	}

	var body map[string]interface{}
	if err := json.Unmarshal(payload, &body); err != nil {
		return payload
	}
	output, _ := body["output"].([]interface{})
	if output == nil {
		output = []interface{}{}
	}

	for _, item := range outputItems {
		output = append(output, item)
	}
	body["output"] = output

	patched, err := json.Marshal(body)
	if err != nil {
		return payload
	}
	return patched
}

func (a *openAIResponsesStreamAccumulator) outputItems() []map[string]interface{} {
	if a == nil {
		return nil
	}

	var output []map[string]interface{}
	text := a.text()
	if strings.TrimSpace(text) != "" {
		responseID := strings.TrimSpace(a.responseID)
		messageID := "msg_stream_aggregated"
		if responseID != "" {
			messageID = "msg_" + responseID + "_stream"
		}
		output = append(output, map[string]interface{}{
			"id":     messageID,
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []interface{}{
				map[string]interface{}{
					"type": "output_text",
					"text": text,
				},
			},
		})
	}

	for _, call := range a.validFunctionCalls() {
		arguments := strings.TrimSpace(call.arguments)
		if arguments == "" {
			arguments = "{}"
		}
		output = append(output, map[string]interface{}{
			"type":      "function_call",
			"id":        call.id,
			"call_id":   call.callID,
			"name":      call.name,
			"arguments": arguments,
			"status":    "completed",
		})
	}
	for _, item := range a.validOutputItems() {
		output = append(output, item)
	}

	return output
}

func (a *openAIResponsesStreamAccumulator) addFunctionCallItem(event map[string]interface{}, item map[string]interface{}) {
	index, ok := responseEventOutputIndex(event)
	if !ok {
		index = a.indexForFunctionCallItem(item)
	}
	if index < 0 {
		index = len(a.functionOrder)
	}
	call := a.ensureFunctionCall(index)
	if id, ok := item["id"].(string); ok && strings.TrimSpace(id) != "" {
		call.id = id
	}
	if callID, ok := item["call_id"].(string); ok && strings.TrimSpace(callID) != "" {
		call.callID = callID
	}
	if name, ok := item["name"].(string); ok && strings.TrimSpace(name) != "" {
		call.name = name
	}
	if arguments, ok := item["arguments"].(string); ok && strings.TrimSpace(arguments) != "" {
		call.arguments = arguments
	}
	if status, ok := item["status"].(string); ok && strings.TrimSpace(status) != "" {
		call.status = status
	}
	if call.callID == "" {
		call.callID = call.id
	}
	if call.id == "" {
		call.id = call.callID
	}
}

func (a *openAIResponsesStreamAccumulator) addFunctionCallArguments(event map[string]interface{}, arguments string, replace bool) {
	index, ok := responseEventOutputIndex(event)
	if !ok {
		index = a.lastFunctionCallIndex()
	}
	if index < 0 {
		index = len(a.functionOrder)
	}
	call := a.ensureFunctionCall(index)
	if replace {
		call.arguments = arguments
	} else {
		call.arguments += arguments
	}
}

func (a *openAIResponsesStreamAccumulator) ensureFunctionCall(index int) *openAIResponsesFunctionCall {
	if a.functionCalls == nil {
		a.functionCalls = make(map[int]*openAIResponsesFunctionCall)
	}
	if call, ok := a.functionCalls[index]; ok {
		return call
	}
	call := &openAIResponsesFunctionCall{index: index}
	a.functionCalls[index] = call
	a.functionOrder = append(a.functionOrder, index)
	return call
}

func (a *openAIResponsesStreamAccumulator) addOutputItem(event map[string]interface{}, item map[string]interface{}) {
	index, ok := responseEventOutputIndex(event)
	if !ok {
		index = a.indexForOutputItem(item)
	}
	if index < 0 {
		index = len(a.outputOrder)
	}
	dst := a.ensureOutputItem(index)
	for key, value := range item {
		dst[key] = value
	}
}

func (a *openAIResponsesStreamAccumulator) addOutputItemInput(event map[string]interface{}, input string, replace bool) {
	index, ok := responseEventOutputIndex(event)
	if !ok {
		index = a.lastOutputItemIndex()
	}
	if index < 0 {
		return
	}
	item := a.ensureOutputItem(index)
	if replace {
		item["input"] = input
		return
	}
	existing, _ := item["input"].(string)
	item["input"] = existing + input
}

func (a *openAIResponsesStreamAccumulator) ensureOutputItem(index int) map[string]interface{} {
	if a.genericItems == nil {
		a.genericItems = make(map[int]map[string]interface{})
	}
	if item, ok := a.genericItems[index]; ok {
		return item
	}
	item := map[string]interface{}{}
	a.genericItems[index] = item
	a.outputOrder = append(a.outputOrder, index)
	return item
}

func (a *openAIResponsesStreamAccumulator) validOutputItems() []map[string]interface{} {
	if a == nil {
		return nil
	}
	items := make([]map[string]interface{}, 0, len(a.outputOrder))
	for _, index := range a.outputOrder {
		item := a.genericItems[index]
		if !hasValidResponsesToolOutputItem(item) {
			continue
		}
		items = append(items, item)
	}
	return items
}

func (a *openAIResponsesStreamAccumulator) indexForOutputItem(item map[string]interface{}) int {
	if a == nil || item == nil {
		return -1
	}
	id, _ := item["id"].(string)
	callID, _ := item["call_id"].(string)
	name, _ := item["name"].(string)
	for _, index := range a.outputOrder {
		existing := a.genericItems[index]
		if existing == nil {
			continue
		}
		if strings.TrimSpace(id) != "" && stringFromInterface(existing["id"]) == id {
			return index
		}
		if strings.TrimSpace(callID) != "" && stringFromInterface(existing["call_id"]) == callID {
			return index
		}
		if strings.TrimSpace(name) != "" && stringFromInterface(existing["name"]) == name {
			return index
		}
	}
	return -1
}

func (a *openAIResponsesStreamAccumulator) lastOutputItemIndex() int {
	if a == nil || len(a.outputOrder) == 0 {
		return -1
	}
	return a.outputOrder[len(a.outputOrder)-1]
}

func (a *openAIResponsesStreamAccumulator) validFunctionCalls() []*openAIResponsesFunctionCall {
	if a == nil {
		return nil
	}
	calls := make([]*openAIResponsesFunctionCall, 0, len(a.functionOrder))
	for _, index := range a.functionOrder {
		call := a.functionCalls[index]
		if call == nil {
			continue
		}
		if strings.TrimSpace(call.id) == "" && strings.TrimSpace(call.callID) == "" && strings.TrimSpace(call.name) == "" {
			continue
		}
		calls = append(calls, call)
	}
	return calls
}

func (a *openAIResponsesStreamAccumulator) indexForFunctionCallItem(item map[string]interface{}) int {
	if a == nil || item == nil {
		return -1
	}
	id, _ := item["id"].(string)
	callID, _ := item["call_id"].(string)
	name, _ := item["name"].(string)
	for _, index := range a.functionOrder {
		call := a.functionCalls[index]
		if call == nil {
			continue
		}
		if strings.TrimSpace(id) != "" && call.id == id {
			return index
		}
		if strings.TrimSpace(callID) != "" && call.callID == callID {
			return index
		}
		if strings.TrimSpace(name) != "" && call.name == name {
			return index
		}
	}
	return -1
}

func (a *openAIResponsesStreamAccumulator) lastFunctionCallIndex() int {
	if a == nil || len(a.functionOrder) == 0 {
		return -1
	}
	return a.functionOrder[len(a.functionOrder)-1]
}

func responseEventOutputIndex(event map[string]interface{}) (int, bool) {
	if event == nil {
		return 0, false
	}
	switch value := event["output_index"].(type) {
	case float64:
		return int(value), true
	case int:
		return value, true
	case json.Number:
		index, err := value.Int64()
		return int(index), err == nil
	default:
		return 0, false
	}
}

func responseOutputItemType(item map[string]interface{}) string {
	if item == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(stringFromInterface(item["type"])))
}

func isResponsesGenericOutputItemType(itemType string) bool {
	itemType = strings.ToLower(strings.TrimSpace(itemType))
	return itemType != "" && itemType != "message" && itemType != "reasoning" && itemType != "function_call"
}

func responseOutputItemText(item map[string]interface{}) string {
	content, ok := item["content"].([]interface{})
	if !ok {
		return ""
	}
	var builder strings.Builder
	for _, rawPart := range content {
		part, ok := rawPart.(map[string]interface{})
		if !ok {
			continue
		}
		builder.WriteString(responseContentPartText(part))
	}
	return builder.String()
}

func responseContentPartText(part map[string]interface{}) string {
	if part == nil {
		return ""
	}
	if text, ok := part["text"].(string); ok {
		return text
	}
	return ""
}

func newOpenAIChatStreamAccumulator() *openAIChatStreamAccumulator {
	return &openAIChatStreamAccumulator{
		choices: make(map[int]*openAIChatStreamChoice),
	}
}

func (a *openAIChatStreamAccumulator) hasData() bool {
	return a != nil && a.seen
}

func (a *openAIChatStreamAccumulator) addChunk(event map[string]interface{}) bool {
	if a == nil || !isOpenAIChatStreamChunk(event) {
		return false
	}
	a.seen = true

	if id, ok := event["id"].(string); ok && id != "" && a.id == "" {
		a.id = id
	}
	if created, ok := event["created"]; ok && a.created == nil {
		a.created = created
	}
	if model, ok := event["model"].(string); ok && model != "" && a.model == "" {
		a.model = model
	}
	if fp, ok := event["system_fingerprint"]; ok && a.systemFingerprint == nil {
		a.systemFingerprint = fp
	}
	if usage, ok := event["usage"].(map[string]interface{}); ok && len(usage) > 0 {
		a.usage = usage
	}

	choices, _ := event["choices"].([]interface{})
	for _, choiceValue := range choices {
		choiceMap, ok := choiceValue.(map[string]interface{})
		if !ok {
			continue
		}
		index := parseTokenNumber(choiceMap["index"])
		choice := a.choice(index)

		if finishReason, ok := choiceMap["finish_reason"]; ok && finishReason != nil {
			choice.finishReason = finishReason
		}

		delta, ok := choiceMap["delta"].(map[string]interface{})
		if !ok {
			continue
		}
		if role, ok := delta["role"].(string); ok && role != "" {
			choice.role = role
		}
		if content, ok := delta["content"].(string); ok && content != "" {
			choice.content += content
		}
		if reasoningContent, ok := delta["reasoning_content"].(string); ok && reasoningContent != "" {
			choice.reasoningContent += reasoningContent
		}
		if toolCalls, ok := delta["tool_calls"].([]interface{}); ok {
			choice.addToolCalls(toolCalls)
		}
	}

	return true
}

func (a *openAIChatStreamAccumulator) choice(index int) *openAIChatStreamChoice {
	choice, ok := a.choices[index]
	if ok {
		return choice
	}
	choice = &openAIChatStreamChoice{
		index:     index,
		role:      "assistant",
		toolCalls: make(map[int]*openAIChatStreamToolCall),
	}
	a.choices[index] = choice
	return choice
}

func (c *openAIChatStreamChoice) addToolCalls(toolCallValues []interface{}) {
	for _, toolCallValue := range toolCallValues {
		toolCallMap, ok := toolCallValue.(map[string]interface{})
		if !ok {
			continue
		}
		index := parseTokenNumber(toolCallMap["index"])
		toolCall := c.toolCall(index)

		if id, ok := toolCallMap["id"].(string); ok && id != "" {
			toolCall.id = id
		}
		if callType, ok := toolCallMap["type"].(string); ok && callType != "" {
			toolCall.callType = callType
		}
		function, _ := toolCallMap["function"].(map[string]interface{})
		if name, ok := function["name"].(string); ok && name != "" {
			toolCall.name = name
		}
		if arguments, ok := function["arguments"].(string); ok && arguments != "" {
			toolCall.arguments += arguments
		}
	}
}

func (c *openAIChatStreamChoice) toolCall(index int) *openAIChatStreamToolCall {
	toolCall, ok := c.toolCalls[index]
	if ok {
		return toolCall
	}
	toolCall = &openAIChatStreamToolCall{index: index, callType: "function"}
	c.toolCalls[index] = toolCall
	return toolCall
}

func (a *openAIChatStreamAccumulator) payload() ([]byte, error) {
	choices := make([]map[string]interface{}, 0, len(a.choices))
	indices := make([]int, 0, len(a.choices))
	for index := range a.choices {
		indices = append(indices, index)
	}
	sort.Ints(indices)

	for _, index := range indices {
		choice := a.choices[index]
		message := map[string]interface{}{
			"role":    choice.role,
			"content": choice.content,
		}
		if choice.reasoningContent != "" {
			message["reasoning_content"] = choice.reasoningContent
		}
		if len(choice.toolCalls) > 0 {
			message["tool_calls"] = choice.toolCallPayloads()
		}
		finishReason := choice.finishReason
		if finishReason == nil {
			finishReason = "stop"
		}
		choices = append(choices, map[string]interface{}{
			"index":         choice.index,
			"message":       message,
			"finish_reason": finishReason,
		})
	}

	created := a.created
	if created == nil {
		created = 0
	}
	payload := map[string]interface{}{
		"id":      a.id,
		"object":  "chat.completion",
		"created": created,
		"model":   a.model,
		"choices": choices,
	}
	if a.usage != nil {
		payload["usage"] = a.usage
	} else {
		payload["usage"] = map[string]interface{}{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		}
	}
	if a.systemFingerprint != nil {
		payload["system_fingerprint"] = a.systemFingerprint
	}

	return json.Marshal(payload)
}

func (c *openAIChatStreamChoice) toolCallPayloads() []map[string]interface{} {
	indices := make([]int, 0, len(c.toolCalls))
	for index := range c.toolCalls {
		indices = append(indices, index)
	}
	sort.Ints(indices)

	payloads := make([]map[string]interface{}, 0, len(indices))
	for _, index := range indices {
		toolCall := c.toolCalls[index]
		payloads = append(payloads, map[string]interface{}{
			"index": toolCall.index,
			"id":    toolCall.id,
			"type":  toolCall.callType,
			"function": map[string]interface{}{
				"name":      toolCall.name,
				"arguments": toolCall.arguments,
			},
		})
	}
	return payloads
}

func isOpenAIChatStreamChunk(event map[string]interface{}) bool {
	if event == nil {
		return false
	}
	object, _ := event["object"].(string)
	if object == "chat.completion.chunk" {
		return true
	}
	if object != "" {
		return false
	}
	choices, ok := event["choices"].([]interface{})
	if !ok {
		return false
	}
	for _, choiceValue := range choices {
		choiceMap, ok := choiceValue.(map[string]interface{})
		if !ok {
			continue
		}
		if _, ok := choiceMap["delta"].(map[string]interface{}); ok {
			return true
		}
	}
	return false
}

// formatRequestSize formats byte size into human-readable string
func formatRequestSize(bytes int) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// transformStreamEvent transforms a single SSE event
func (p *Proxy) transformStreamEvent(eventData []byte, trans transformer.Transformer, transformerName string, streamCtx *transformer.StreamContext) ([]byte, error) {
	eventData = normalizeSSEDataEventTypes(eventData)
	// Use the unified interface method instead of type assertion switch
	// All transformers now implement TransformResponseWithContext
	return trans.TransformResponseWithContext(eventData, true, streamCtx)
}

func shouldEnsureResponsesStreamCompletion(transformerName string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(transformerName)), "cx_resp_")
}

func shouldInjectClaudeMessageDeltaUsage(transformerName string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(transformerName)), "cc_")
}

func buildSyntheticResponsesCompletionEvent(responseID string, inputTokens int, outputTokens int, outputText string) []byte {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		responseID = "resp_ccnexus_synthetic"
	}

	output := make([]map[string]interface{}, 0, 1)
	if outputText != "" {
		output = append(output, map[string]interface{}{
			"type":   "message",
			"role":   "assistant",
			"status": "completed",
			"content": []map[string]interface{}{
				{
					"type": "output_text",
					"text": outputText,
				},
			},
		})
	}
	payload := map[string]interface{}{
		"type": "response.completed",
		"response": map[string]interface{}{
			"id":     responseID,
			"object": "response",
			"status": "completed",
			"usage": map[string]interface{}{
				"input_tokens":  inputTokens,
				"output_tokens": outputTokens,
				"total_tokens":  inputTokens + outputTokens,
			},
			"output": output,
		},
	}
	encoded, _ := json.Marshal(payload)
	return []byte(fmt.Sprintf("data: %s\n\n", encoded))
}

func updateResponsesStreamCompletionState(eventData []byte, responseID *string, completed *bool) {
	scanner := bufio.NewScanner(bytes.NewReader(eventData))
	sseEventType := ""
	for scanner.Scan() {
		line := scanner.Text()
		if parsedEventType := sseEventTypeFromLine(line); parsedEventType != "" {
			sseEventType = parsedEventType
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		jsonData := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if jsonData == "" || jsonData == "[DONE]" {
			continue
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(jsonData), &event); err != nil {
			continue
		}
		ensureSSEEventType(event, sseEventType)

		if eventType, _ := event["type"].(string); eventType == "response.completed" && completed != nil {
			*completed = true
		}
		if response, ok := event["response"].(map[string]interface{}); ok {
			if id, ok := response["id"].(string); ok && strings.TrimSpace(id) != "" && responseID != nil && *responseID == "" {
				*responseID = id
			}
		}
		if id, ok := event["id"].(string); ok && strings.TrimSpace(id) != "" && responseID != nil && *responseID == "" {
			*responseID = id
		}
	}
}

func normalizeSSEDataEventTypes(eventData []byte) []byte {
	if len(eventData) == 0 {
		return eventData
	}

	lines := strings.Split(string(eventData), "\n")
	eventType := ""
	changed := false
	for i, line := range lines {
		if parsedEventType := sseEventTypeFromLine(line); parsedEventType != "" {
			eventType = parsedEventType
			continue
		}
		if eventType == "" {
			continue
		}

		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		jsonData := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if jsonData == "" || jsonData == "[DONE]" || !strings.HasPrefix(jsonData, "{") {
			continue
		}

		var event map[string]interface{}
		if err := json.Unmarshal([]byte(jsonData), &event); err != nil {
			continue
		}
		if existingType, _ := event["type"].(string); strings.TrimSpace(existingType) != "" {
			continue
		}
		event["type"] = eventType

		normalized, err := json.Marshal(event)
		if err != nil {
			continue
		}
		lines[i] = "data: " + string(normalized)
		changed = true
	}
	if !changed {
		return eventData
	}
	return []byte(strings.Join(lines, "\n"))
}

func sseEventTypeFromLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "event:") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
}

func ensureSSEEventType(event map[string]interface{}, eventType string) {
	if event == nil || strings.TrimSpace(eventType) == "" {
		return
	}
	if existingType, _ := event["type"].(string); strings.TrimSpace(existingType) != "" {
		return
	}
	event["type"] = strings.TrimSpace(eventType)
}

// extractTokensFromEvent extracts token counts from SSE event
func (p *Proxy) extractTokensFromEvent(eventData []byte, inputTokens, outputTokens *int) {
	scanner := bufio.NewScanner(bytes.NewReader(eventData))
	sseEventType := ""
	for scanner.Scan() {
		line := scanner.Text()
		if parsedEventType := sseEventTypeFromLine(line); parsedEventType != "" {
			sseEventType = parsedEventType
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		jsonData := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if jsonData == "" || jsonData == "[DONE]" {
			continue
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(jsonData), &event); err != nil {
			continue
		}
		ensureSSEEventType(event, sseEventType)

		applyUsage := func(usage map[string]interface{}) {
			in, out := extractInputOutputTokens(usage)
			if in > 0 {
				*inputTokens = in
			}
			if out > 0 {
				*outputTokens = out
			}
		}

		// Claude-style events
		eventType, _ := event["type"].(string)
		if eventType == "message_start" {
			if message, ok := event["message"].(map[string]interface{}); ok {
				if usage, ok := message["usage"].(map[string]interface{}); ok {
					applyUsage(usage)
				}
			}
		} else if eventType == "message_delta" {
			if usage, ok := event["usage"].(map[string]interface{}); ok {
				applyUsage(usage)
			}
		}

		// OpenAI Responses-style events
		if response, ok := event["response"].(map[string]interface{}); ok {
			if usage, ok := response["usage"].(map[string]interface{}); ok {
				applyUsage(usage)
			}
		}

		// OpenAI Chat chunk-style usage (top-level)
		if usage, ok := event["usage"].(map[string]interface{}); ok {
			applyUsage(usage)
		}

		// Some providers wrap payloads with object=...
		if obj, ok := event["object"].(string); ok && strings.Contains(obj, "chat.completion") {
			if usage, ok := event["usage"].(map[string]interface{}); ok {
				applyUsage(usage)
			}
		}
	}
}

// extractTextFromEvent extracts text content from transformed event
// Enhanced to support both delta.text and content_block_delta formats
func (p *Proxy) extractTextFromEvent(transformedEvent []byte, outputText *strings.Builder) {
	scanner := bufio.NewScanner(bytes.NewReader(transformedEvent))
	sseEventType := ""
	for scanner.Scan() {
		line := scanner.Text()
		if parsedEventType := sseEventTypeFromLine(line); parsedEventType != "" {
			sseEventType = parsedEventType
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		jsonData := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(jsonData), &event); err != nil {
			continue
		}
		ensureSSEEventType(event, sseEventType)

		eventType, _ := event["type"].(string)

		// Handle content_block_delta format (from some third-party APIs)
		if eventType == "content_block_delta" {
			if delta, ok := event["delta"].(map[string]interface{}); ok {
				if text, ok := delta["text"].(string); ok {
					outputText.WriteString(text)
				}
			}
		} else if delta, ok := event["delta"].(map[string]interface{}); ok {
			// Handle standard delta.text format
			if text, ok := delta["text"].(string); ok {
				outputText.WriteString(text)
			}
		}

		// Handle OpenAI Responses stream text delta format
		if eventType == "response.output_text.delta" {
			if delta, ok := event["delta"].(string); ok {
				outputText.WriteString(delta)
			}
		}
		if eventType == "response.output_text.done" && outputText.Len() == 0 {
			if text, ok := event["text"].(string); ok {
				outputText.WriteString(text)
			}
		}
		if eventType == "response.content_part.done" && outputText.Len() == 0 {
			if part, ok := event["part"].(map[string]interface{}); ok {
				outputText.WriteString(responseContentPartText(part))
			}
		}
		if eventType == "response.output_item.done" && outputText.Len() == 0 {
			if item, ok := event["item"].(map[string]interface{}); ok {
				outputText.WriteString(responseOutputItemText(item))
			}
		}

		// Handle OpenAI Chat stream chunk format (choices[].delta.content)
		if choices, ok := event["choices"].([]interface{}); ok {
			for _, choice := range choices {
				choiceMap, ok := choice.(map[string]interface{})
				if !ok {
					continue
				}
				delta, ok := choiceMap["delta"].(map[string]interface{})
				if !ok {
					continue
				}
				if text, ok := delta["content"].(string); ok {
					outputText.WriteString(text)
				}
			}
		}
	}
}

// isMessageStopEvent checks if the event is a message_stop event
func (p *Proxy) isMessageStopEvent(eventData []byte) bool {
	scanner := bufio.NewScanner(bytes.NewReader(eventData))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		jsonData := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(jsonData), &event); err != nil {
			continue
		}

		eventType, _ := event["type"].(string)
		if eventType == "message_stop" {
			return true
		}
	}
	return false
}

// decompressGzip decompresses gzip-encoded response body
func decompressGzip(body io.ReadCloser) ([]byte, error) {
	gzipReader, err := gzip.NewReader(body)
	if err != nil {
		return nil, err
	}
	defer gzipReader.Close()
	return io.ReadAll(gzipReader)
}
