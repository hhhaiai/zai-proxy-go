package internal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	bp "zai-proxy/internal/browserproxy"
	"github.com/google/uuid"
)

// ==================== Responses API Types ====================

// ResponsesRequest is the OpenAI Responses API request format.
type ResponsesRequest struct {
	Model  string      `json:"model"`
	Input  interface{} `json:"input"` // string or []ResponsesInputItem
	Stream bool        `json:"stream,omitempty"`
}

// ResponsesInputItem represents a single input item.
type ResponsesInputItem struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string or []interface{}
}

// ResponsesOutputItem represents a single output item in the response.
type ResponsesOutputItem struct {
	Type    string                   `json:"type"`
	ID      string                   `json:"id,omitempty"`
	Role    string                   `json:"role,omitempty"`
	Content []ResponsesOutputContent `json:"content,omitempty"`
	Status  string                   `json:"status,omitempty"`
}

// ResponsesOutputContent represents text output in a response item.
type ResponsesOutputContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ResponsesAPIResponse is the non-stream response format.
type ResponsesAPIResponse struct {
	ID        string                `json:"id"`
	Object    string                `json:"object"`
	CreatedAt int64                 `json:"created_at"`
	Model     string                `json:"model"`
	Output    []ResponsesOutputItem `json:"output"`
	Usage     ResponsesUsage        `json:"usage"`
	Status    string                `json:"status"`
}

// ResponsesUsage represents token usage.
type ResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ==================== Conversion ====================

// convertResponsesInput converts Responses API input to internal Message format.
func convertResponsesInput(input interface{}) ([]Message, error) {
	switch v := input.(type) {
	case string:
		// Simple string input → single user message
		return []Message{{Role: "user", Content: v}}, nil

	case []interface{}:
		var messages []Message
		for _, item := range v {
			obj, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := obj["role"].(string)
			if role == "" {
				role = "user"
			}

			content := obj["content"]
			switch c := content.(type) {
			case string:
				messages = append(messages, Message{Role: role, Content: c})
			case []interface{}:
				// Multi-part content
				var parts []interface{}
				for _, part := range c {
					if p, ok := part.(map[string]interface{}); ok {
						parts = append(parts, p)
					}
				}
				messages = append(messages, Message{Role: role, Content: parts})
			default:
				messages = append(messages, Message{Role: role, Content: content})
			}
		}
		return messages, nil

	default:
		return nil, fmt.Errorf("invalid input type: expected string or array")
	}
}

// ==================== Handler ====================

// HandleResponses handles /v1/responses — OpenAI Responses API.
// Converts to internal Message format and reuses the same upstream logic.
func HandleResponses(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		writeResponsesError(w, http.StatusUnauthorized, "Missing API key")
		return
	}

	isAnonymous := false

	if token == "free" {
		isAnonymous = true
		actualToken, err := GetTokenManager().GetNextToken()
		if err != nil {
			LogError("[Responses] Failed to get token from manager: %v", err)
			writeResponsesError(w, http.StatusInternalServerError, "No available tokens")
			return
		}
		token = actualToken
	} else if token == "managed" {
		actualToken, err := GetTokenManager().GetNextToken()
		if err != nil {
			LogError("[Responses] Failed to get token from manager: %v", err)
			writeResponsesError(w, http.StatusInternalServerError, "No available tokens")
			return
		}
		token = actualToken
	}

	var req ResponsesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeResponsesError(w, http.StatusBadRequest, "Invalid JSON request body")
		return
	}

	if req.Input == nil {
		writeResponsesError(w, http.StatusBadRequest, "input is required")
		return
	}

	messages, err := convertResponsesInput(req.Input)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.Model == "" {
		req.Model = "GLM-4.7"
	}

	responseID := fmt.Sprintf("resp_%s", uuid.New().String()[:24])
	outputMsgID := fmt.Sprintf("msg_%s", uuid.New().String()[:24])

	// Anonymous mode: use cached session if available, otherwise browser proxy
	if isAnonymous {
		if session := GetCaptchaSession(); session != nil {
			LogInfo("[Responses] Using cached session for direct API call")
			token = session.Token
		} else {
			LogInfo("[Responses] No cached session, routing through browser proxy")
			handleResponsesViaBrowserProxy(w, req.Model, messages, req.Stream, responseID, outputMsgID)
			return
		}
	}

	resp, modelName, err := makeUpstreamRequest(token, messages, req.Model)
	if err != nil {
		LogError("[Responses] Upstream request failed: %v", err)
		writeResponsesError(w, http.StatusBadGateway, "Upstream service error")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		bodyStr := string(body)
		if len(bodyStr) > 500 {
			bodyStr = bodyStr[:500]
		}
		LogError("[Responses] Upstream error: status=%d, body=%s", resp.StatusCode, bodyStr)
		writeResponsesError(w, resp.StatusCode, "Upstream error")
		return
	}

	if req.Stream {
		handleResponsesStream(w, resp.Body, responseID, outputMsgID, modelName)
	} else {
		handleResponsesNonStream(w, resp.Body, responseID, outputMsgID, modelName)
	}
}

// ==================== Non-stream ====================

func handleResponsesNonStream(w http.ResponseWriter, body io.ReadCloser, responseID, outputMsgID, modelName string) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var chunks []string
	var reasoningChunks []string
	searchRefFilter := NewSearchRefFilter()
	thinkingFilter := &ThinkingFilter{}

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}

		var upstream UpstreamData
		if err := json.Unmarshal([]byte(payload), &upstream); err != nil {
			continue
		}
		if upstreamErr := upstream.GetError(); upstreamErr != nil {
			LogError("[Responses][NonStream] Upstream error: %s", upstreamErr.Detail)
			errMsg := upstreamErr.Detail
			if errMsg == "" {
				errMsg = "Upstream error: " + upstreamErr.Code
			}
			if upstreamErr.Code == "FRONTEND_CAPTCHA_REQUIRED" {
				port := "8000"
				if Cfg != nil && Cfg.Port != "" {
					port = Cfg.Port
				}
				errMsg = fmt.Sprintf("Captcha required. Please visit http://localhost:%s/captcha in your browser to solve captcha first.", port)
			}
			chunks = append(chunks, "[Error] "+errMsg)
			break
		}
		if upstream.Data.Phase == "done" {
			break
		}

		editContent := upstream.GetEditContent()

		// Skip search results, image search, MCP blocks
		if editContent != "" && IsSearchResultContent(editContent) {
			if results := ParseSearchResults(editContent); len(results) > 0 {
				searchRefFilter.AddSearchResults(results)
			}
			continue
		}
		if editContent != "" && (strings.Contains(editContent, `"search_image"`) || strings.Contains(editContent, `"mcp"`)) {
			textBefore := ExtractTextBeforeGlmBlock(editContent)
			if textBefore != "" {
				chunks = append(chunks, textBefore)
			}
			continue
		}
		if editContent != "" && IsSearchToolCall(editContent, upstream.Data.Phase) {
			continue
		}

		// Thinking
		if upstream.Data.Phase == "thinking" && upstream.Data.DeltaContent != "" {
			if IsGLM5Model(modelName) {
				reasoningChunks = append(reasoningChunks, upstream.Data.DeltaContent)
			} else {
				if thinkingFilter.lastPhase != "" && thinkingFilter.lastPhase != "thinking" {
					thinkingFilter.ResetForNewRound()
				}
				thinkingFilter.lastPhase = "thinking"
				if rc := thinkingFilter.ProcessThinking(upstream.Data.DeltaContent); rc != "" {
					thinkingFilter.lastOutputChunk = rc
					reasoningChunks = append(reasoningChunks, rc)
				}
			}
			continue
		}
		if upstream.Data.Phase != "" {
			thinkingFilter.lastPhase = upstream.Data.Phase
		}

		// Answer content
		if upstream.Data.Phase == "answer" && upstream.Data.DeltaContent != "" {
			chunks = append(chunks, upstream.Data.DeltaContent)
		} else if upstream.Data.Phase == "answer" && editContent != "" {
			if strings.Contains(editContent, "</details>") {
				if rc := thinkingFilter.ExtractIncrementalThinking(editContent); rc != "" {
					reasoningChunks = append(reasoningChunks, rc)
				}
				if idx := strings.Index(editContent, "</details>"); idx != -1 {
					after := editContent[idx+len("</details>"):]
					if strings.HasPrefix(after, "\n") {
						after = after[1:]
					}
					chunks = append(chunks, after)
				}
			}
		} else if (upstream.Data.Phase == "other" || upstream.Data.Phase == "tool_call") && editContent != "" {
			chunks = append(chunks, editContent)
		}
	}

	fullContent := strings.Join(chunks, "")
	fullContent = searchRefFilter.Process(fullContent) + searchRefFilter.Flush()
	fullReasoning := strings.Join(reasoningChunks, "")
	fullReasoning = searchRefFilter.Process(fullReasoning) + searchRefFilter.Flush()

	if fullContent == "" {
		LogError("[Responses] Non-stream: no content received")
	}

	outputTokens := (len(fullContent) + len(fullReasoning)) / 4

	// Build Responses API output
	var outputItems []ResponsesOutputItem
	outputItems = append(outputItems, ResponsesOutputItem{
		Type: "message",
		ID:   outputMsgID,
		Role: "assistant",
		Content: []ResponsesOutputContent{
			{Type: "output_text", Text: fullContent},
		},
		Status: "completed",
	})

	response := ResponsesAPIResponse{
		ID:        responseID,
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Model:     modelName,
		Output:    outputItems,
		Usage: ResponsesUsage{
			InputTokens:  0,
			OutputTokens: outputTokens,
			TotalTokens:  outputTokens,
		},
		Status: "completed",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// ==================== Stream ====================

func handleResponsesStream(w http.ResponseWriter, body io.ReadCloser, responseID, outputMsgID, modelName string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	// 1. response.created
	sendResponsesSSE(w, flusher, "response.created", map[string]interface{}{
		"type": "response.created",
		"response": map[string]interface{}{
			"id":         responseID,
			"object":     "response",
			"created_at": time.Now().Unix(),
			"model":      modelName,
			"output":     []interface{}{},
			"usage":      map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
			"status":     "in_progress",
		},
	})

	// 2. response.in_progress
	sendResponsesSSE(w, flusher, "response.in_progress", map[string]interface{}{
		"type": "response.in_progress",
		"response": map[string]interface{}{
			"id":         responseID,
			"object":     "response",
			"created_at": time.Now().Unix(),
			"model":      modelName,
			"output":     []interface{}{},
			"usage":      map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
			"status":     "in_progress",
		},
	})

	// 3. response.output_item.added
	sendResponsesSSE(w, flusher, "response.output_item.added", map[string]interface{}{
		"type":  "response.output_item.added",
		"output_index": 0,
		"item": map[string]interface{}{
			"type":    "message",
			"id":      outputMsgID,
			"role":    "assistant",
			"content": []interface{}{},
			"status":  "in_progress",
		},
	})

	// 4. response.content_part.added
	sendResponsesSSE(w, flusher, "response.content_part.added", map[string]interface{}{
		"type":        "response.content_part.added",
		"output_index": 0,
		"content_index": 0,
		"part": map[string]interface{}{
			"type": "output_text",
			"text": "",
		},
	})

	// Stream content
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	searchRefFilter := NewSearchRefFilter()
	thinkingFilter := &ThinkingFilter{}
	outputTokens := 0

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}

		var upstream UpstreamData
		if err := json.Unmarshal([]byte(payload), &upstream); err != nil {
			continue
		}
		if upstreamErr := upstream.GetError(); upstreamErr != nil {
			LogError("[Responses][Stream] Upstream error: %s", upstreamErr.Detail)
			sendResponsesSSE(w, flusher, "response.output_text.delta", map[string]interface{}{
				"type":  "response.output_text.delta",
				"output_index": 0,
				"content_index": 0,
				"delta": "[Error] " + upstreamErr.Detail,
			})
			break
		}
		if upstream.Data.Phase == "done" {
			break
		}

		editContent := upstream.GetEditContent()

		// Skip search/tool metadata
		if editContent != "" && IsSearchResultContent(editContent) {
			if results := ParseSearchResults(editContent); len(results) > 0 {
				searchRefFilter.AddSearchResults(results)
			}
			continue
		}
		if editContent != "" && IsSearchToolCall(editContent, upstream.Data.Phase) {
			continue
		}

		// Skip thinking for Responses stream (simplification — could be extended)
		if upstream.Data.Phase == "thinking" {
			thinkingFilter.lastPhase = "thinking"
			continue
		}
		if upstream.Data.Phase != "" {
			thinkingFilter.lastPhase = upstream.Data.Phase
		}

		content := ""
		if upstream.Data.Phase == "answer" && upstream.Data.DeltaContent != "" {
			content = upstream.Data.DeltaContent
		} else if upstream.Data.Phase == "answer" && editContent != "" {
			if strings.Contains(editContent, "</details>") {
				if idx := strings.Index(editContent, "</details>"); idx != -1 {
					after := editContent[idx+len("</details>"):]
					if strings.HasPrefix(after, "\n") {
						after = after[1:]
					}
					content = after
				}
			}
		} else if (upstream.Data.Phase == "other" || upstream.Data.Phase == "tool_call") && editContent != "" {
			content = editContent
		}

		if content == "" {
			continue
		}
		content = searchRefFilter.Process(content)
		if content == "" {
			continue
		}

		outputTokens += len(content) / 4
		sendResponsesSSE(w, flusher, "response.output_text.delta", map[string]interface{}{
			"type":           "response.output_text.delta",
			"output_index":   0,
			"content_index":  0,
			"delta":          content,
		})
	}

	// Flush any remaining search ref content
	if remaining := searchRefFilter.Flush(); remaining != "" {
		sendResponsesSSE(w, flusher, "response.output_text.delta", map[string]interface{}{
			"type":           "response.output_text.delta",
			"output_index":   0,
			"content_index":  0,
			"delta":          remaining,
		})
	}

	// 5. response.content_part.done
	sendResponsesSSE(w, flusher, "response.content_part.done", map[string]interface{}{
		"type":           "response.content_part.done",
		"output_index":   0,
		"content_index":  0,
		"part": map[string]interface{}{
			"type": "output_text",
			"text": "",
		},
	})

	// 6. response.output_item.done
	sendResponsesSSE(w, flusher, "response.output_item.done", map[string]interface{}{
		"type":         "response.output_item.done",
		"output_index": 0,
		"item": map[string]interface{}{
			"type":    "message",
			"id":      outputMsgID,
			"role":    "assistant",
			"content": []interface{}{},
			"status":  "completed",
		},
	})

	// 7. response.completed
	sendResponsesSSE(w, flusher, "response.completed", map[string]interface{}{
		"type": "response.completed",
		"response": map[string]interface{}{
			"id":         responseID,
			"object":     "response",
			"created_at": time.Now().Unix(),
			"model":      modelName,
			"output": []map[string]interface{}{
				{
					"type": "message",
					"id":   outputMsgID,
					"role": "assistant",
					"content": []map[string]interface{}{
						{"type": "output_text", "text": ""},
					},
					"status": "completed",
				},
			},
			"usage": map[string]int{
				"input_tokens":  0,
				"output_tokens": outputTokens,
				"total_tokens":  outputTokens,
			},
			"status": "completed",
		},
	})
}

// ==================== Browser Proxy ====================

func handleResponsesViaBrowserProxy(w http.ResponseWriter, clientModel string, messages []Message, stream bool, responseID, outputMsgID string) {
	if !bp.IsReady() {
		writeResponsesError(w, http.StatusServiceUnavailable, "Browser pool not ready")
		return
	}

	userMsg := ""
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			userMsg, _ = messages[i].ParseContent()
			break
		}
	}
	if userMsg == "" {
		writeResponsesError(w, http.StatusBadRequest, "No user message found")
		return
	}

	LogInfo("[Responses/BrowserProxy] Processing, pool=%d", bp.PoolSize())

	worker := bp.Acquire()
	defer bp.Release(worker)

	answer, err := bp.ProcessChat(worker, userMsg)
	if err != nil {
		LogError("[Responses/BrowserProxy] Error: %v", err)
		writeResponsesError(w, http.StatusBadGateway, fmt.Sprintf("Browser proxy error: %v", err))
		return
	}

	LogInfo("[Responses/BrowserProxy] Done: %d chars", len(answer))

	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming not supported", http.StatusInternalServerError)
			return
		}

		sendResponsesSSE(w, flusher, "response.created", map[string]interface{}{
			"type": "response.created",
			"response": map[string]interface{}{
				"id": responseID, "object": "response",
				"created_at": time.Now().Unix(), "model": clientModel,
				"output": []interface{}{},
				"usage":  map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
				"status": "in_progress",
			},
		})

		sendResponsesSSE(w, flusher, "response.output_item.added", map[string]interface{}{
			"type": "response.output_item.added", "output_index": 0,
			"item": map[string]interface{}{
				"type": "message", "id": outputMsgID, "role": "assistant",
				"content": []interface{}{}, "status": "in_progress",
			},
		})

		sendResponsesSSE(w, flusher, "response.content_part.added", map[string]interface{}{
			"type": "response.content_part.added", "output_index": 0, "content_index": 0,
			"part": map[string]interface{}{"type": "output_text", "text": ""},
		})

		sendResponsesSSE(w, flusher, "response.output_text.delta", map[string]interface{}{
			"type": "response.output_text.delta", "output_index": 0, "content_index": 0,
			"delta": answer,
		})

		sendResponsesSSE(w, flusher, "response.content_part.done", map[string]interface{}{
			"type": "response.content_part.done", "output_index": 0, "content_index": 0,
			"part": map[string]interface{}{"type": "output_text", "text": ""},
		})

		sendResponsesSSE(w, flusher, "response.output_item.done", map[string]interface{}{
			"type": "response.output_item.done", "output_index": 0,
			"item": map[string]interface{}{
				"type": "message", "id": outputMsgID, "role": "assistant",
				"content": []interface{}{}, "status": "completed",
			},
		})

		sendResponsesSSE(w, flusher, "response.completed", map[string]interface{}{
			"type": "response.completed",
			"response": map[string]interface{}{
				"id": responseID, "object": "response",
				"created_at": time.Now().Unix(), "model": clientModel,
				"output": []map[string]interface{}{
					{"type": "message", "id": outputMsgID, "role": "assistant",
						"content": []map[string]interface{}{{"type": "output_text", "text": answer}},
						"status": "completed"},
				},
				"usage":  map[string]int{"input_tokens": 0, "output_tokens": len(answer) / 4, "total_tokens": len(answer) / 4},
				"status": "completed",
			},
		})
	} else {
		outputTokens := len(answer) / 4
		response := ResponsesAPIResponse{
			ID:        responseID,
			Object:    "response",
			CreatedAt: time.Now().Unix(),
			Model:     clientModel,
			Output: []ResponsesOutputItem{
				{
					Type: "message", ID: outputMsgID, Role: "assistant",
					Content: []ResponsesOutputContent{{Type: "output_text", Text: answer}},
					Status:  "completed",
				},
			},
			Usage:  ResponsesUsage{OutputTokens: outputTokens, TotalTokens: outputTokens},
			Status: "completed",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

// ==================== Helpers ====================

func sendResponsesSSE(w http.ResponseWriter, flusher http.Flusher, eventType string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, jsonData)
	flusher.Flush()
}

func writeResponsesError(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    "invalid_request_error",
		},
	})
}

