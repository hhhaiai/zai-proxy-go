package internal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	bp "zai-proxy/internal/browserproxy"
	"github.com/corpix/uarand"
	"github.com/google/uuid"
)

func extractLatestUserContent(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			text, _ := messages[i].ParseContent()
			return text
		}
	}
	return ""
}

func extractAllImageURLs(messages []Message) []string {
	var allImageURLs []string
	for _, msg := range messages {
		_, imageURLs := msg.ParseContent()
		allImageURLs = append(allImageURLs, imageURLs...)
	}
	return allImageURLs
}

func makeUpstreamRequest(token string, messages []Message, model string) (*http.Response, string, error) {
	payload, err := DecodeJWTPayload(token)
	if err != nil || payload == nil {
		return nil, "", fmt.Errorf("invalid token")
	}

	userID := payload.ID
	chatID := uuid.New().String()
	timestamp := time.Now().UnixMilli()
	requestID := uuid.New().String()
	userMsgID := uuid.New().String()

	targetModel := GetTargetModel(model)
	latestUserContent := extractLatestUserContent(messages)
	imageURLs := extractAllImageURLs(messages)

	signature := GenerateSignature(userID, requestID, latestUserContent, timestamp)

	url := fmt.Sprintf("https://chat.z.ai/api/v2/chat/completions?timestamp=%d&requestId=%s&user_id=%s&version=0.0.1&platform=web&token=%s&current_url=%s&pathname=%s&signature_timestamp=%d",
		timestamp, requestID, userID, token,
		fmt.Sprintf("https://chat.z.ai/c/%s", chatID),
		fmt.Sprintf("/c/%s", chatID),
		timestamp)

	// Thinking and search defaults: always enabled unless explicitly disabled
	enableThinking := true
	if strings.HasSuffix(model, "-nothinking") {
		enableThinking = false
	}
	autoWebSearch := true
	if targetModel == "glm-4.5v" || targetModel == "glm-4.6v" {
		autoWebSearch = false
	}

	// MCP servers: always include advanced-search for text models
	var mcpServers []string
	if targetModel == "glm-4.6v" {
		mcpServers = []string{"vlm-image-search", "vlm-image-recognition", "vlm-image-processing"}
	} else {
		mcpServers = []string{"advanced-search"}
	}

	urlToFileID := make(map[string]string)
	var filesData []map[string]interface{}
	if len(imageURLs) > 0 {
		files, _ := UploadImages(token, imageURLs)
		for i, f := range files {
			if i < len(imageURLs) {
				urlToFileID[imageURLs[i]] = f.ID
			}
			filesData = append(filesData, map[string]interface{}{
				"type":            f.Type,
				"file":            f.File,
				"id":              f.ID,
				"url":             f.URL,
				"name":            f.Name,
				"status":          f.Status,
				"size":            f.Size,
				"error":           f.Error,
				"itemId":          f.ItemID,
				"media":           f.Media,
				"ref_user_msg_id": userMsgID,
			})
		}
	}

	var upstreamMessages []map[string]interface{}
	for _, msg := range messages {
		upstreamMessages = append(upstreamMessages, msg.ToUpstreamMessage(urlToFileID))
	}

	body := map[string]interface{}{
		"stream":           true,
		"model":            targetModel,
		"messages":         upstreamMessages,
		"signature_prompt": latestUserContent,
		"params":           map[string]interface{}{},
		"features": map[string]interface{}{
			"image_generation": false,
			"web_search":       false,
			"auto_web_search":  autoWebSearch,
			"preview_mode":     true,
			"enable_thinking":  enableThinking,
		},
		"chat_id": chatID,
		"id":      uuid.New().String(),
	}

	if len(mcpServers) > 0 {
		body["mcp_servers"] = mcpServers
	}

	if len(filesData) > 0 {
		body["files"] = filesData
		body["current_user_message_id"] = userMsgID
	}

	// captcha_verify_param: include if available from /captcha page
	if captchaParam, err := GetCaptchaVerifyParam(); err == nil && captchaParam != "" {
		body["captcha_verify_param"] = captchaParam
		LogDebug("[Upstream] Including captcha_verify_param")
	}

	bodyBytes, _ := json.Marshal(body)

	// Debug: log request body
	LogDebug("[Upstream] Request body: %s", string(bodyBytes))

	req, err := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, "", err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-FE-Version", GetFeVersion())
	req.Header.Set("X-Signature", signature)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Origin", "https://chat.z.ai")
	req.Header.Set("Referer", fmt.Sprintf("https://chat.z.ai/c/%s", uuid.New().String()))
	req.Header.Set("User-Agent", uarand.GetRandom())

	LogInfo("[Upstream] Sending request: model=%s, target=%s", model, targetModel)

	client := GetSharedHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		LogError("[Upstream] Request error: %v", err)
		return nil, "", err
	}

	LogInfo("[Upstream] Response status: %d", resp.StatusCode)

	return resp, targetModel, nil
}

type UpstreamData struct {
	Type string `json:"type"`
	Data struct {
		DeltaContent string `json:"delta_content"`
		EditContent  string `json:"edit_content"`
		Phase        string `json:"phase"`
		Done         bool   `json:"done"`
		Error        *struct {
			Code            string `json:"code"`
			Detail          string `json:"detail"`
			CaptchaErrorType string `json:"captcha_error_type"`
			ErrorCode       string `json:"error_code"`
		} `json:"error,omitempty"`
		Data *struct {
			Done  bool `json:"done"`
			Error *struct {
				Code            string `json:"code"`
				Detail          string `json:"detail"`
				CaptchaErrorType string `json:"captcha_error_type"`
				ErrorCode       string `json:"error_code"`
			} `json:"error,omitempty"`
		} `json:"data,omitempty"`
	} `json:"data"`
}

// GetError returns the error from upstream data (checks both levels)
func (u *UpstreamData) GetError() *struct {
	Code            string `json:"code"`
	Detail          string `json:"detail"`
	CaptchaErrorType string `json:"captcha_error_type"`
	ErrorCode       string `json:"error_code"`
} {
	if u.Data.Error != nil {
		return u.Data.Error
	}
	if u.Data.Data != nil && u.Data.Data.Error != nil {
		return u.Data.Data.Error
	}
	return nil
}

func (u *UpstreamData) GetEditContent() string {
	editContent := u.Data.EditContent
	if editContent == "" {
		return ""
	}

	if len(editContent) > 0 && editContent[0] == '"' {
		var unescaped string
		if err := json.Unmarshal([]byte(editContent), &unescaped); err == nil {
			LogDebug("[GetEditContent] Unescaped edit_content from JSON string")
			return unescaped
		}
	}

	return editContent
}

type ThinkingFilter struct {
	hasSeenFirstThinking bool
	buffer               string
	lastOutputChunk      string
	lastPhase            string
	thinkingRoundCount   int
}

func (f *ThinkingFilter) ProcessThinking(deltaContent string) string {
	if !f.hasSeenFirstThinking {
		f.hasSeenFirstThinking = true
		if idx := strings.Index(deltaContent, "> "); idx != -1 {
			deltaContent = deltaContent[idx+2:]
		} else {
			return ""
		}
	}

	content := f.buffer + deltaContent
	f.buffer = ""

	content = strings.ReplaceAll(content, "\n> ", "\n")

	if strings.HasSuffix(content, "\n>") {
		f.buffer = "\n>"
		return content[:len(content)-2]
	}
	if strings.HasSuffix(content, "\n") {
		f.buffer = "\n"
		return content[:len(content)-1]
	}

	return content
}

func (f *ThinkingFilter) Flush() string {
	result := f.buffer
	f.buffer = ""
	return result
}

func (f *ThinkingFilter) ExtractCompleteThinking(editContent string) string {
	startIdx := strings.Index(editContent, "> ")
	if startIdx == -1 {
		return ""
	}
	startIdx += 2

	endIdx := strings.Index(editContent, "\n</details>")
	if endIdx == -1 {
		return ""
	}

	content := editContent[startIdx:endIdx]
	content = strings.ReplaceAll(content, "\n> ", "\n")
	return content
}

func (f *ThinkingFilter) ExtractIncrementalThinking(editContent string) string {
	completeThinking := f.ExtractCompleteThinking(editContent)
	if completeThinking == "" {
		return ""
	}

	if f.lastOutputChunk == "" {
		return completeThinking
	}

	idx := strings.Index(completeThinking, f.lastOutputChunk)
	if idx == -1 {
		return completeThinking
	}

	incrementalPart := completeThinking[idx+len(f.lastOutputChunk):]
	return incrementalPart
}

func (f *ThinkingFilter) ResetForNewRound() {
	f.lastOutputChunk = ""
	f.hasSeenFirstThinking = false
}

func HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Track whether this is anonymous mode
	isAnonymous := false

	// 使用Token管理器获取实际使用的token
	if token == "free" {
		isAnonymous = true
		actualToken, err := GetTokenManager().GetNextToken()
		if err != nil {
			LogError("Failed to get token from manager: %v", err)
			http.Error(w, "No available tokens", http.StatusInternalServerError)
			return
		}
		if len(actualToken) > 30 {
			LogDebug("TokenManager returned: %s...", actualToken[:30])
		} else {
			LogDebug("TokenManager returned: %s", actualToken)
		}
		token = actualToken
	} else if token == "managed" {
		actualToken, err := GetTokenManager().GetNextToken()
		if err != nil {
			LogError("Failed to get token from manager: %v", err)
			http.Error(w, "No available tokens", http.StatusInternalServerError)
			return
		}
		token = actualToken
	}

	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// Anonymous mode only supports GLM-4.7
	if isAnonymous {
		req.Model = "GLM-4.7"
		LogInfo("[Anonymous] Forced model to GLM-4.7")
	}

	if req.Model == "" {
		req.Model = "GLM-4.7"
	}

	isGLM5 := IsGLM5Model(req.Model)

	// For anonymous mode, use browser proxy
	if isAnonymous {
		handleBrowserProxyRequest(w, req)
		return
	}

	resp, modelName, err := makeUpstreamRequest(token, req.Messages, req.Model)
	if err != nil {
		LogError("Upstream request failed: %v", err)
		if len(token) > 50 {
			LogDebug("Failed token (first 50): %s", token[:50])
		} else {
			LogDebug("Failed token: %s", token)
		}
		http.Error(w, "Upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Debug: log response headers
	LogDebug("[Upstream] Response headers: %v", resp.Header)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		bodyStr := string(body)
		if len(bodyStr) > 500 {
			bodyStr = bodyStr[:500]
		}
		LogError("Upstream error: status=%d, body=%s", resp.StatusCode, bodyStr)

		// Token失效处理
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			// 标记当前token无效
			if token != "free" && token != "managed" {
				GetTokenManager().InvalidateToken(token, fmt.Sprintf("HTTP %d", resp.StatusCode))
			}
		}

		http.Error(w, "Upstream error", resp.StatusCode)
		return
	}

	completionID := fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:29])

	if req.Stream {
		handleStreamResponse(w, resp.Body, completionID, modelName, isGLM5)
	} else {
		handleNonStreamResponse(w, resp.Body, completionID, modelName, isGLM5)
	}
}

func handleStreamResponse(w http.ResponseWriter, body io.ReadCloser, completionID, modelName string, isGLM5 bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	hasContent := false
	searchRefFilter := NewSearchRefFilter()
	thinkingFilter := &ThinkingFilter{}
	pendingSourcesMarkdown := ""
	pendingImageSearchMarkdown := ""
	totalContentOutputLength := 0 // 记录已输出的 content 字符长度

	for scanner.Scan() {
		line := scanner.Text()
		LogDebug("[Upstream] %s", line)

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

		// Check for upstream errors (e.g., captcha required)
		if upstreamErr := upstream.GetError(); upstreamErr != nil {
			LogError("[Upstream] Error: code=%s, detail=%s", upstreamErr.Code, upstreamErr.Detail)
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
			chunk := ChatCompletionChunk{
				ID:      completionID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   modelName,
				Choices: []Choice{{
					Index: 0,
					Delta: Delta{Content: "[Error] " + errMsg},
					FinishReason: func() *string { s := "stop"; return &s }(),
				}},
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}

		if upstream.Data.Phase == "done" {
			break
		}

		if upstream.Data.Phase == "thinking" && upstream.Data.DeltaContent != "" {
			if isGLM5 {
				// GLM-5 thinking content is plain text, no blockquote wrapping
				reasoningContent := upstream.Data.DeltaContent
				reasoningContent = searchRefFilter.Process(reasoningContent)
				if reasoningContent != "" {
					hasContent = true
					chunk := ChatCompletionChunk{
						ID:      completionID,
						Object:  "chat.completion.chunk",
						Created: time.Now().Unix(),
						Model:   modelName,
						Choices: []Choice{{
							Index:        0,
							Delta:        Delta{ReasoningContent: reasoningContent},
							FinishReason: nil,
						}},
					}
					data, _ := json.Marshal(chunk)
					fmt.Fprintf(w, "data: %s\n\n", data)
					flusher.Flush()
				}
				continue
			}

			isNewThinkingRound := false
			if thinkingFilter.lastPhase != "" && thinkingFilter.lastPhase != "thinking" {
				thinkingFilter.ResetForNewRound()
				thinkingFilter.thinkingRoundCount++
				isNewThinkingRound = true
			}
			thinkingFilter.lastPhase = "thinking"

			reasoningContent := thinkingFilter.ProcessThinking(upstream.Data.DeltaContent)

			if isNewThinkingRound && thinkingFilter.thinkingRoundCount > 1 && reasoningContent != "" {
				reasoningContent = "\n\n" + reasoningContent
			}

			if reasoningContent != "" {
				thinkingFilter.lastOutputChunk = reasoningContent
				reasoningContent = searchRefFilter.Process(reasoningContent)

				if reasoningContent != "" {
					hasContent = true
					chunk := ChatCompletionChunk{
						ID:      completionID,
						Object:  "chat.completion.chunk",
						Created: time.Now().Unix(),
						Model:   modelName,
						Choices: []Choice{{
							Index:        0,
							Delta:        Delta{ReasoningContent: reasoningContent},
							FinishReason: nil,
						}},
					}
					data, _ := json.Marshal(chunk)
					fmt.Fprintf(w, "data: %s\n\n", data)
					flusher.Flush()
				}
			}
			continue
		}

		if upstream.Data.Phase != "" {
			thinkingFilter.lastPhase = upstream.Data.Phase
		}

		editContent := upstream.GetEditContent()
		if editContent != "" && IsSearchResultContent(editContent) {
			if results := ParseSearchResults(editContent); len(results) > 0 {
				searchRefFilter.AddSearchResults(results)
				pendingSourcesMarkdown = searchRefFilter.GetSearchResultsMarkdown()
			}
			continue
		}
		if editContent != "" && strings.Contains(editContent, `"search_image"`) {
			textBeforeBlock := ExtractTextBeforeGlmBlock(editContent)
			if textBeforeBlock != "" {
				textBeforeBlock = searchRefFilter.Process(textBeforeBlock)
				if textBeforeBlock != "" {
					hasContent = true
					chunk := ChatCompletionChunk{
						ID:      completionID,
						Object:  "chat.completion.chunk",
						Created: time.Now().Unix(),
						Model:   modelName,
						Choices: []Choice{{
							Index:        0,
							Delta:        Delta{Content: textBeforeBlock},
							FinishReason: nil,
						}},
					}
					data, _ := json.Marshal(chunk)
					fmt.Fprintf(w, "data: %s\n\n", data)
					flusher.Flush()
				}
			}
			if results := ParseImageSearchResults(editContent); len(results) > 0 {
				pendingImageSearchMarkdown = FormatImageSearchResults(results)
			}
			continue
		}
		if editContent != "" && strings.Contains(editContent, `"mcp"`) {
			textBeforeBlock := ExtractTextBeforeGlmBlock(editContent)
			if textBeforeBlock != "" {
				textBeforeBlock = searchRefFilter.Process(textBeforeBlock)
				if textBeforeBlock != "" {
					hasContent = true
					chunk := ChatCompletionChunk{
						ID:      completionID,
						Object:  "chat.completion.chunk",
						Created: time.Now().Unix(),
						Model:   modelName,
						Choices: []Choice{{
							Index:        0,
							Delta:        Delta{Content: textBeforeBlock},
							FinishReason: nil,
						}},
					}
					data, _ := json.Marshal(chunk)
					fmt.Fprintf(w, "data: %s\n\n", data)
					flusher.Flush()
				}
			}
			continue
		}
		if editContent != "" && IsSearchToolCall(editContent, upstream.Data.Phase) {
			continue
		}

		if pendingSourcesMarkdown != "" {
			hasContent = true
			chunk := ChatCompletionChunk{
				ID:      completionID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   modelName,
				Choices: []Choice{{
					Index:        0,
					Delta:        Delta{Content: pendingSourcesMarkdown},
					FinishReason: nil,
				}},
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			pendingSourcesMarkdown = ""
		}
		if pendingImageSearchMarkdown != "" {
			hasContent = true
			chunk := ChatCompletionChunk{
				ID:      completionID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   modelName,
				Choices: []Choice{{
					Index:        0,
					Delta:        Delta{Content: pendingImageSearchMarkdown},
					FinishReason: nil,
				}},
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			pendingImageSearchMarkdown = ""
		}

		content := ""
		reasoningContent := ""

		if thinkingRemaining := thinkingFilter.Flush(); thinkingRemaining != "" {
			thinkingFilter.lastOutputChunk = thinkingRemaining
			processedRemaining := searchRefFilter.Process(thinkingRemaining)
			if processedRemaining != "" {
				hasContent = true
				chunk := ChatCompletionChunk{
					ID:      completionID,
					Object:  "chat.completion.chunk",
					Created: time.Now().Unix(),
					Model:   modelName,
					Choices: []Choice{{
						Index:        0,
						Delta:        Delta{ReasoningContent: processedRemaining},
						FinishReason: nil,
					}},
				}
				data, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
		}

		if pendingSourcesMarkdown != "" && thinkingFilter.hasSeenFirstThinking {
			hasContent = true
			chunk := ChatCompletionChunk{
				ID:      completionID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   modelName,
				Choices: []Choice{{
					Index:        0,
					Delta:        Delta{ReasoningContent: pendingSourcesMarkdown},
					FinishReason: nil,
				}},
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			pendingSourcesMarkdown = ""
		}

		if upstream.Data.Phase == "answer" && upstream.Data.DeltaContent != "" {
			content = upstream.Data.DeltaContent
		} else if upstream.Data.Phase == "answer" && editContent != "" {
			if strings.Contains(editContent, "</details>") {
				reasoningContent = thinkingFilter.ExtractIncrementalThinking(editContent)

				if idx := strings.Index(editContent, "</details>"); idx != -1 {
					afterDetails := editContent[idx+len("</details>"):]
					if strings.HasPrefix(afterDetails, "\n") {
						content = afterDetails[1:]
					} else {
						content = afterDetails
					}
					totalContentOutputLength = len([]rune(content))
				}
			}
		} else if (upstream.Data.Phase == "other" || upstream.Data.Phase == "tool_call") && editContent != "" {
			fullContent := editContent
			fullContentRunes := []rune(fullContent)

			if len(fullContentRunes) > totalContentOutputLength {
				content = string(fullContentRunes[totalContentOutputLength:])
				totalContentOutputLength = len(fullContentRunes)
			} else {
				content = fullContent
			}
		}

		if reasoningContent != "" {
			reasoningContent = searchRefFilter.Process(reasoningContent) + searchRefFilter.Flush()
		}
		if reasoningContent != "" {
			hasContent = true
			chunk := ChatCompletionChunk{
				ID:      completionID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   modelName,
				Choices: []Choice{{
					Index:        0,
					Delta:        Delta{ReasoningContent: reasoningContent},
					FinishReason: nil,
				}},
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}

		if content == "" {
			continue
		}

		content = searchRefFilter.Process(content)
		if content == "" {
			continue
		}

		hasContent = true
		if upstream.Data.Phase == "answer" && upstream.Data.DeltaContent != "" {
			totalContentOutputLength += len([]rune(content))
		}

		chunk := ChatCompletionChunk{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   modelName,
			Choices: []Choice{{
				Index:        0,
				Delta:        Delta{Content: content},
				FinishReason: nil,
			}},
		}

		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	if err := scanner.Err(); err != nil {
		LogError("[Upstream] scanner error: %v", err)
	}

	if remaining := searchRefFilter.Flush(); remaining != "" {
		hasContent = true
		chunk := ChatCompletionChunk{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   modelName,
			Choices: []Choice{{
				Index:        0,
				Delta:        Delta{Content: remaining},
				FinishReason: nil,
			}},
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	if !hasContent {
		LogError("Stream response 200 but no content received")
	}

	stopReason := "stop"
	finalChunk := ChatCompletionChunk{
		ID:      completionID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   modelName,
		Choices: []Choice{{
			Index:        0,
			Delta:        Delta{},
			FinishReason: &stopReason,
		}},
	}

	data, _ := json.Marshal(finalChunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func handleNonStreamResponse(w http.ResponseWriter, body io.ReadCloser, completionID, modelName string, isGLM5 bool) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	var chunks []string
	var reasoningChunks []string
	thinkingFilter := &ThinkingFilter{}
	searchRefFilter := NewSearchRefFilter()
	hasThinking := false
	pendingSourcesMarkdown := ""
	pendingImageSearchMarkdown := ""
	lineCount := 0

	for scanner.Scan() {
		line := scanner.Text()
		lineCount++

		// Debug: log first few lines
		if lineCount <= 10 {
			LogDebug("[NonStream] Line %d: %s", lineCount, line)
		}

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

		// Check for upstream errors (e.g., captcha required)
		if upstreamErr := upstream.GetError(); upstreamErr != nil {
			LogError("[NonStream] Upstream error: code=%s, detail=%s", upstreamErr.Code, upstreamErr.Detail)
			errMsg := upstreamErr.Detail
			if upstreamErr.Code == "FRONTEND_CAPTCHA_REQUIRED" {
				port := "8000"
				if Cfg != nil && Cfg.Port != "" {
					port = Cfg.Port
				}
				errMsg = fmt.Sprintf("Captcha required. Please visit http://localhost:%s/captcha in your browser to solve captcha first.", port)
			}
			stopReason := "stop"
			response := ChatCompletionResponse{
				ID:      completionID,
				Object:  "chat.completion",
				Created: time.Now().Unix(),
				Model:   modelName,
				Choices: []Choice{{
					Index: 0,
					Message: &MessageResp{
						Role:    "assistant",
						Content: "[Error] " + errMsg,
					},
					FinishReason: &stopReason,
				}},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)
			return
		}

		if upstream.Data.Phase == "done" {
			break
		}

		if upstream.Data.Phase == "thinking" && upstream.Data.DeltaContent != "" {
			if isGLM5 {
				// GLM-5 thinking content is plain text, no blockquote wrapping
				hasThinking = true
				reasoningChunks = append(reasoningChunks, upstream.Data.DeltaContent)
				continue
			}

			if thinkingFilter.lastPhase != "" && thinkingFilter.lastPhase != "thinking" {
				thinkingFilter.ResetForNewRound()
				thinkingFilter.thinkingRoundCount++
				if thinkingFilter.thinkingRoundCount > 1 {
					reasoningChunks = append(reasoningChunks, "\n\n")
				}
			}
			thinkingFilter.lastPhase = "thinking"

			hasThinking = true
			reasoningContent := thinkingFilter.ProcessThinking(upstream.Data.DeltaContent)
			if reasoningContent != "" {
				thinkingFilter.lastOutputChunk = reasoningContent
				reasoningChunks = append(reasoningChunks, reasoningContent)
			}
			continue
		}

		if upstream.Data.Phase != "" {
			thinkingFilter.lastPhase = upstream.Data.Phase
		}

		editContent := upstream.GetEditContent()
		if editContent != "" && IsSearchResultContent(editContent) {
			if results := ParseSearchResults(editContent); len(results) > 0 {
				searchRefFilter.AddSearchResults(results)
				pendingSourcesMarkdown = searchRefFilter.GetSearchResultsMarkdown()
			}
			continue
		}
		if editContent != "" && strings.Contains(editContent, `"search_image"`) {
			textBeforeBlock := ExtractTextBeforeGlmBlock(editContent)
			if textBeforeBlock != "" {
				chunks = append(chunks, textBeforeBlock)
			}
			// 解析图片搜索结果
			if results := ParseImageSearchResults(editContent); len(results) > 0 {
				pendingImageSearchMarkdown = FormatImageSearchResults(results)
			}
			continue
		}
		if editContent != "" && strings.Contains(editContent, `"mcp"`) {
			textBeforeBlock := ExtractTextBeforeGlmBlock(editContent)
			if textBeforeBlock != "" {
				chunks = append(chunks, textBeforeBlock)
			}
			continue
		}
		if editContent != "" && IsSearchToolCall(editContent, upstream.Data.Phase) {
			continue
		}

		if pendingSourcesMarkdown != "" {
			if hasThinking {
				reasoningChunks = append(reasoningChunks, pendingSourcesMarkdown)
			} else {
				chunks = append(chunks, pendingSourcesMarkdown)
			}
			pendingSourcesMarkdown = ""
		}
		if pendingImageSearchMarkdown != "" {
			chunks = append(chunks, pendingImageSearchMarkdown)
			pendingImageSearchMarkdown = ""
		}

		content := ""
		if upstream.Data.Phase == "answer" && upstream.Data.DeltaContent != "" {
			content = upstream.Data.DeltaContent
		} else if upstream.Data.Phase == "answer" && editContent != "" {
			if strings.Contains(editContent, "</details>") {
				reasoningContent := thinkingFilter.ExtractIncrementalThinking(editContent)
				if reasoningContent != "" {
					reasoningChunks = append(reasoningChunks, reasoningContent)
				}

				if idx := strings.Index(editContent, "</details>"); idx != -1 {
					afterDetails := editContent[idx+len("</details>"):]
					if strings.HasPrefix(afterDetails, "\n") {
						content = afterDetails[1:]
					} else {
						content = afterDetails
					}
				}
			}
		} else if (upstream.Data.Phase == "other" || upstream.Data.Phase == "tool_call") && editContent != "" {
			content = editContent
		}

		if content != "" {
			chunks = append(chunks, content)
		}
	}

	fullContent := strings.Join(chunks, "")
	fullContent = searchRefFilter.Process(fullContent) + searchRefFilter.Flush()
	fullReasoning := strings.Join(reasoningChunks, "")
	fullReasoning = searchRefFilter.Process(fullReasoning) + searchRefFilter.Flush()

	// Debug: log content
	LogDebug("[NonStream] Total lines: %d, chunks: %d, reasoningChunks: %d", lineCount, len(chunks), len(reasoningChunks))
	LogDebug("[NonStream] Full content length: %d, fullReasoning length: %d", len(fullContent), len(fullReasoning))
	if len(fullContent) > 0 {
		LogDebug("[NonStream] Content preview: %s", fullContent[:min(200, len(fullContent))])
	}

	if fullContent == "" {
		LogError("Non-stream response 200 but no content received")
	}

	stopReason := "stop"
	response := ChatCompletionResponse{
		ID:      completionID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   modelName,
		Choices: []Choice{{
			Index: 0,
			Message: &MessageResp{
				Role:             "assistant",
				Content:          fullContent,
				ReasoningContent: fullReasoning,
			},
			FinishReason: &stopReason,
		}},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func HandleModels(w http.ResponseWriter, r *http.Request) {
	var models []ModelInfo
	for _, id := range ModelList {
		models = append(models, ModelInfo{
			ID:      id,
			Object:  "model",
			OwnedBy: "z.ai",
		})
	}

	response := ModelsResponse{
		Object: "list",
		Data:   models,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleBrowserProxyRequest forwards anonymous requests via the Go chromedp pool.
func handleBrowserProxyRequest(w http.ResponseWriter, req ChatRequest) {
	if !bp.IsReady() {
		http.Error(w, "Browser pool not ready, try again later", http.StatusServiceUnavailable)
		return
	}

	// Extract user message
	userMsg := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			userMsg, _ = req.Messages[i].ParseContent()
			break
		}
	}
	if userMsg == "" {
		http.Error(w, "No user message found", http.StatusBadRequest)
		return
	}

	LogInfo("[BrowserProxy] Processing: model=%s, pool=%d", req.Model, bp.PoolSize())

	worker := bp.Acquire()
	defer bp.Release(worker)

	answer, err := bp.ProcessChat(worker, userMsg)
	if err != nil {
		LogError("[BrowserProxy] Error: %v", err)
		http.Error(w, fmt.Sprintf("Browser proxy error: %v", err), http.StatusBadGateway)
		return
	}

	LogInfo("[BrowserProxy] Done: %d chars", len(answer))

	completionID := fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:29])

	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		chunk := ChatCompletionChunk{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   req.Model,
			Choices: []Choice{{
				Index: 0,
				Delta: Delta{Content: answer},
			}},
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		done := ChatCompletionChunk{
			ID: completionID, Object: "chat.completion.chunk",
			Created: time.Now().Unix(), Model: req.Model,
			Choices: []Choice{{Index: 0, FinishReason: strPtr("stop")}},
		}
		dd, _ := json.Marshal(done)
		fmt.Fprintf(w, "data: %s\n\n", dd)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		if ok { flusher.Flush() }
	} else {
		resp := ChatCompletionResponse{
			ID: completionID, Object: "chat.completion",
			Created: time.Now().Unix(), Model: req.Model,
			Choices: []Choice{{
				Index: 0,
				Message: &MessageResp{Role: "assistant", Content: answer},
				FinishReason: strPtr("stop"),
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

func strPtr(s string) *string { return &s }
