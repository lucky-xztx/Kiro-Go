package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/providers"
	"kiro-go/providers/codex"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// isCodexModel reports whether a model name should be routed to a Codex (ChatGPT)
// upstream rather than Kiro. GPT family models go to Codex; everything else goes
// to Kiro (the default). The model name is already lowercased by the caller.
func isCodexModel(model string) bool {
	lower := strings.ToLower(model)
	return strings.HasPrefix(lower, "gpt-") || strings.HasPrefix(lower, "o1") || strings.HasPrefix(lower, "o3") || strings.HasPrefix(lower, "o4")
}

// codexModelForRequest maps a client-side model name to the Codex model ID.
// If the model already looks like a valid Codex model, return as-is.
func codexModelForRequest(model string) string {
	// Pass through known Codex models directly
	lower := strings.ToLower(model)
	for _, m := range codexModels {
		if lower == m {
			return model
		}
	}
	// Default fallback
	return "gpt-5.5"
}

var codexModels = []string{
	"gpt-5.5",
	"gpt-5.4",
	"gpt-5.3-codex",
	"gpt-5.3-codex-spark",
}

// getNextCodexAccount returns the next available Codex upstream account.
func (h *Handler) getNextCodexAccount(excluded map[string]bool) *config.Account {
	for attempt := 0; attempt < len(h.pool.GetAllAccounts())+1; attempt++ {
		acc := h.pool.GetNextExcluding(excluded)
		if acc == nil {
			return nil
		}
		if providers.Normalize(acc.Upstream) == "codex" {
			return acc
		}
		excluded[acc.ID] = true
	}
	return nil
}

// ensureCodexToken ensures the Codex account has a valid access token.
// It refreshes via the ChatGPT OAuth flow if needed.
func (h *Handler) ensureCodexToken(account *config.Account) error {
	if account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
		return nil
	}

	h.tokenRefreshMu.Lock()
	defer h.tokenRefreshMu.Unlock()

	// Re-check after acquiring lock
	if latest := h.pool.GetByID(account.ID); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		if account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
			return nil
		}
	}

	tr, err := codex.RefreshTokens(account.RefreshToken)
	if err != nil {
		return err
	}

	expiresAt := time.Now().Unix() + int64(tr.ExpiresIn) - 60
	account.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		account.RefreshToken = tr.RefreshToken
	}
	account.ExpiresAt = expiresAt

	h.pool.UpdateToken(account.ID, tr.AccessToken, tr.RefreshToken, expiresAt)
	config.UpdateAccountToken(account.ID, tr.AccessToken, tr.RefreshToken, expiresAt)

	return nil
}

// handleOpenAIChatViaCodex routes an OpenAI Chat Completions request to Codex.
func (h *Handler) handleOpenAIChatViaCodex(w http.ResponseWriter, req *OpenAIRequest, apiKeyID string) {
	model := codexModelForRequest(req.Model)

	// Convert messages to codex format
	codexMsgs := make([]codex.OpenAIChatMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		codexMsgs = append(codexMsgs, codex.OpenAIChatMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCalls:  convertToolCalls(m.ToolCalls),
			ToolCallID: m.ToolCallID,
		})
	}

	// Convert tools
	var toolsJSON json.RawMessage
	if len(req.Tools) > 0 {
		toolsJSON, _ = json.Marshal(convertOpenAIToolsToCodex(req.Tools))
	}

	codexReq := codex.ConvertOpenAIToCodex(codexMsgs, model, req.Stream, toolsJSON, "medium")

	if req.Stream {
		h.handleCodexOpenAIStream(w, codexReq, model, apiKeyID)
	} else {
		h.handleCodexOpenAINonStream(w, codexReq, model, apiKeyID)
	}
}

func convertToolCalls(tcs []ToolCall) []codex.OpenAIToolCall {
	if len(tcs) == 0 {
		return nil
	}
	out := make([]codex.OpenAIToolCall, len(tcs))
	for i, tc := range tcs {
		out[i].ID = tc.ID
		out[i].Type = tc.Type
		out[i].Function.Name = tc.Function.Name
		out[i].Function.Arguments = tc.Function.Arguments
	}
	return out
}

func convertOpenAIToolsToCodex(tools []OpenAITool) []interface{} {
	var out []interface{}
	for _, t := range tools {
		if t.Type == "function" {
			out = append(out, map[string]interface{}{
				"type":        "function",
				"name":        t.Function.Name,
				"description": t.Function.Description,
				"parameters":  t.Function.Parameters,
			})
		}
	}
	return out
}

// handleCodexOpenAIStream handles streaming OpenAI Chat Completions via Codex.
func (h *Handler) handleCodexOpenAIStream(w http.ResponseWriter, req *codex.CodexRequest, model string, apiKeyID string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	chatID := "chatcmpl-" + uuid.New().String()
	excluded := make(map[string]bool)
	var lastErr error

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account := h.getNextCodexAccount(excluded)
		if account == nil {
			break
		}
		if err := h.ensureCodexToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		// Extract ChatGPT account ID from the stored field or parse from refresh token
		accountID := account.UserId

		var textBuilder strings.Builder
		var toolCalls []ToolCall
		var toolCallIndex int

		sendChunk := func(content string, finishReason *string) {
			chunk := map[string]interface{}{
				"id":      chatID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   model,
				"choices": []map[string]interface{}{{
					"index":         0,
					"delta":         map[string]string{"content": content},
					"finish_reason": finishReason,
				}},
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			flusher.Flush()
		}

		sendToolCallChunk := func(tc ToolCall) {
			chunk := map[string]interface{}{
				"id":      chatID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   model,
				"choices": []map[string]interface{}{{
					"index": 0,
					"delta": map[string]interface{}{
						"tool_calls": []map[string]interface{}{{
							"index": toolCallIndex,
							"id":    tc.ID,
							"type":  "function",
							"function": map[string]string{
								"name":      tc.Function.Name,
								"arguments": tc.Function.Arguments,
							},
						}},
					},
					"finish_reason": nil,
				}},
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			flusher.Flush()
			toolCallIndex++
		}

		parser := &codex.StreamEventParser{}
		cb := &codex.StreamCallback{
			OnEvent: func(eventType string, data json.RawMessage) {
				parser.ProcessEvent(eventType, data)

				switch eventType {
				case "response.output_text.delta":
					var evt struct {
						Delta string `json:"delta"`
					}
					if json.Unmarshal(data, &evt) == nil && evt.Delta != "" {
						textBuilder.WriteString(evt.Delta)
						sendChunk(evt.Delta, nil)
					}

				case "response.output_item.added":
					var evt struct {
						Item struct {
							Type   string `json:"type"`
							CallID string `json:"call_id"`
							Name   string `json:"name"`
							ID     string `json:"id"`
						} `json:"item"`
					}
					if json.Unmarshal(data, &evt) == nil && evt.Item.Type == "function_call" {
						tc := ToolCall{
							ID:   evt.Item.CallID,
							Type: "function",
						}
						tc.Function.Name = evt.Item.Name
						toolCalls = append(toolCalls, tc)
					}

				case "response.function_call_arguments.delta":
					var evt struct {
						Delta string `json:"delta"`
					}
					if json.Unmarshal(data, &evt) == nil && len(toolCalls) > 0 {
						last := &toolCalls[len(toolCalls)-1]
						last.Function.Arguments += evt.Delta
					}

				case "response.output_item.done":
					var evt struct {
						Item struct {
							Type      string `json:"type"`
							CallID    string `json:"call_id"`
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"item"`
					}
					if json.Unmarshal(data, &evt) == nil && evt.Item.Type == "function_call" {
						// Find or append
						found := false
						for i := range toolCalls {
							if toolCalls[i].ID == evt.Item.CallID {
								if evt.Item.Arguments != "" {
									toolCalls[i].Function.Arguments = evt.Item.Arguments
								}
								found = true
								break
							}
						}
						if !found {
							tc := ToolCall{ID: evt.Item.CallID, Type: "function"}
							tc.Function.Name = evt.Item.Name
							tc.Function.Arguments = evt.Item.Arguments
							toolCalls = append(toolCalls, tc)
						}
					}

				case "response.completed":
					// Send any pending tool calls
					for _, tc := range toolCalls {
						sendToolCallChunk(tc)
					}
					// Send finish
					finishReason := "stop"
					if len(toolCalls) > 0 {
						finishReason = "tool_calls"
					}
					chunk := map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]interface{}{},
							"finish_reason": finishReason,
						}},
					}
					data, _ := json.Marshal(chunk)
					fmt.Fprintf(w, "data: %s\n\n", string(data))
					flusher.Flush()

					// Send usage
					usageChunk := map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{}},
						"usage": map[string]interface{}{
							"prompt_tokens":     0,
							"completion_tokens": textBuilder.Len(),
							"total_tokens":      textBuilder.Len(),
						},
					}
					usageData, _ := json.Marshal(usageChunk)
					fmt.Fprintf(w, "data: %s\n\n", string(usageData))
					flusher.Flush()

					fmt.Fprintf(w, "data: [DONE]\n\n")
					flusher.Flush()
				}
			},
		}

		err := codex.CallCodexAPI(account.AccessToken, accountID, req, cb)
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			logger.Warnf("[Codex] Account %s failed: %v", account.Email, err)
			continue
		}

		h.pool.RecordSuccess(account.ID)
		h.logCallSuccess(apiKeyID, "/v1/chat/completions", account, model, 0, textBuilder.Len(), 0)
		return
	}

	// All attempts failed
	errMsg := "No available Codex accounts"
	if lastErr != nil {
		errMsg = lastErr.Error()
	}
	h.sendOpenAIError(w, 502, "server_error", errMsg)
}

// handleCodexOpenAINonStream handles non-streaming OpenAI Chat Completions via Codex.
func (h *Handler) handleCodexOpenAINonStream(w http.ResponseWriter, req *codex.CodexRequest, model string, apiKeyID string) {
	excluded := make(map[string]bool)
	var lastErr error

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account := h.getNextCodexAccount(excluded)
		if account == nil {
			break
		}
		if err := h.ensureCodexToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		accountID := account.UserId
		parser := &codex.StreamEventParser{}
		cb := &codex.StreamCallback{
			OnEvent: func(eventType string, data json.RawMessage) {
				parser.ProcessEvent(eventType, data)
			},
		}

		err := codex.CallCodexAPI(account.AccessToken, accountID, req, cb)
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			logger.Warnf("[Codex] Account %s failed: %v", account.Email, err)
			continue
		}

		text := parser.TextBuilder.String()
		finishReason := "stop"
		if len(parser.ToolCalls) > 0 {
			finishReason = "tool_calls"
		}

		// Build OpenAI response
		var toolCalls []ToolCall
		for _, tc := range parser.ToolCalls {
			toolCalls = append(toolCalls, ToolCall{
				ID:   tc.CallID,
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: tc.Name, Arguments: tc.Arguments},
			})
		}

		resp := map[string]interface{}{
			"id":      "chatcmpl-" + uuid.New().String(),
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{{
				"index": 0,
				"message": map[string]interface{}{
					"role":       "assistant",
					"content":    text,
					"tool_calls": toolCallsOrNil(toolCalls),
				},
				"finish_reason": finishReason,
			}},
			"usage": map[string]interface{}{
				"prompt_tokens":     0,
				"completion_tokens": len(text),
				"total_tokens":      len(text),
			},
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)

		h.pool.RecordSuccess(account.ID)
		h.logCallSuccess(apiKeyID, "/v1/chat/completions", account, model, 0, len(text), 0)
		return
	}

	errMsg := "No available Codex accounts"
	if lastErr != nil {
		errMsg = lastErr.Error()
	}
	h.sendOpenAIError(w, 502, "server_error", errMsg)
}

func toolCallsOrNil(tcs []ToolCall) interface{} {
	if len(tcs) == 0 {
		return nil
	}
	return tcs
}

// handleResponsesViaCodex routes a Responses API request to Codex upstream.
func (h *Handler) handleResponsesViaCodex(
	w http.ResponseWriter, respReq *ResponsesRequest, openaiReq *OpenAIRequest,
	apiKeyID, respID string, storedInput json.RawMessage, storeResponse bool,
) {
	model := codexModelForRequest(openaiReq.Model)

	codexMsgs := make([]codex.OpenAIChatMessage, 0, len(openaiReq.Messages))
	for _, m := range openaiReq.Messages {
		codexMsgs = append(codexMsgs, codex.OpenAIChatMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCalls:  convertToolCalls(m.ToolCalls),
			ToolCallID: m.ToolCallID,
		})
	}

	var toolsJSON json.RawMessage
	if len(respReq.Tools) > 0 {
		toolsJSON, _ = json.Marshal(convertOpenAIToolsToCodex(respReq.Tools))
	}

	codexReq := codex.ConvertOpenAIToCodex(codexMsgs, model, respReq.Stream, toolsJSON, "medium")

	if respReq.Stream {
		h.handleCodexResponsesStream(w, codexReq, model, apiKeyID, respID, respReq, storedInput, storeResponse)
	} else {
		h.handleCodexResponsesNonStream(w, codexReq, model, apiKeyID, respID, respReq, storedInput, storeResponse)
	}
}

func (h *Handler) handleCodexResponsesNonStream(
	w http.ResponseWriter, req *codex.CodexRequest, model string,
	apiKeyID, respID string, respReq *ResponsesRequest,
	storedInput json.RawMessage, storeResponse bool,
) {
	excluded := make(map[string]bool)
	var lastErr error

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account := h.getNextCodexAccount(excluded)
		if account == nil {
			break
		}
		if err := h.ensureCodexToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		parser := &codex.StreamEventParser{}
		cb := &codex.StreamCallback{
			OnEvent: func(eventType string, data json.RawMessage) {
				parser.ProcessEvent(eventType, data)
			},
		}

		err := codex.CallCodexAPI(account.AccessToken, account.UserId, req, cb)
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		text := parser.TextBuilder.String()
		outputTokens := len(text)

		var outputItems []ResponseOutputItem
		if text != "" {
			outputItems = append(outputItems, ResponseOutputItem{
				ID:     "msg_" + uuid.New().String(),
				Type:   "message",
				Role:   "assistant",
				Status: "completed",
				Content: []ResponseContentPart{
					{Type: "output_text", Text: text},
				},
			})
		}
		for _, tc := range parser.ToolCalls {
			outputItems = append(outputItems, ResponseOutputItem{
				ID:        "fc_" + uuid.New().String(),
				Type:      "function_call",
				Status:    "completed",
				CallID:    tc.CallID,
				Name:      tc.Name,
				Arguments: tc.Arguments,
			})
		}

		respObj := &ResponsesObject{
			ID:                 respID,
			Object:             "response",
			CreatedAt:          time.Now().Unix(),
			Status:             "completed",
			Model:              model,
			Output:             outputItems,
			Usage:              ResponsesUsage{InputTokens: 0, OutputTokens: outputTokens, TotalTokens: outputTokens},
			PreviousResponseID: respReq.PreviousResponseID,
			Metadata:           respReq.Metadata,
			StoredInput:        storedInput,
			StoredInstr:        respReq.Instructions,
		}

		if storeResponse {
			if saveErr := saveResponse(respObj); saveErr != nil {
				logResponsesPersistFailure(respObj.ID, saveErr)
			}
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(respObj)

		h.pool.RecordSuccess(account.ID)
		h.logCallSuccess(apiKeyID, "/v1/responses", account, model, 0, outputTokens, 0)
		return
	}

	errMsg := "No available Codex accounts"
	if lastErr != nil {
		errMsg = lastErr.Error()
	}
	h.sendOpenAIError(w, 502, "server_error", errMsg)
}

func (h *Handler) handleCodexResponsesStream(
	w http.ResponseWriter, req *codex.CodexRequest, model string,
	apiKeyID, respID string, respReq *ResponsesRequest,
	storedInput json.RawMessage, storeResponse bool,
) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	send := func(eventName string, payload interface{}) {
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, string(data))
		flusher.Flush()
	}

	createdAt := time.Now().Unix()
	initial := &ResponsesObject{
		ID:                 respID,
		Object:             "response",
		CreatedAt:          createdAt,
		Status:             "in_progress",
		Model:              model,
		Output:             []ResponseOutputItem{},
		Usage:              ResponsesUsage{},
		PreviousResponseID: respReq.PreviousResponseID,
		Metadata:           respReq.Metadata,
	}
	send("response.created", map[string]interface{}{
		"type":     "response.created",
		"response": initial,
	})

	excluded := make(map[string]bool)
	var lastErr error

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account := h.getNextCodexAccount(excluded)
		if account == nil {
			break
		}
		if err := h.ensureCodexToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		send("response.in_progress", map[string]interface{}{
			"type":     "response.in_progress",
			"response": initial,
		})

		var textBuilder strings.Builder
		var outputItems []ResponseOutputItem
		msgID := "msg_" + uuid.New().String()

		cb := &codex.StreamCallback{
			OnEvent: func(eventType string, data json.RawMessage) {
				switch eventType {
				case "response.output_text.delta":
					var evt struct {
						Delta string `json:"delta"`
					}
					if json.Unmarshal(data, &evt) == nil && evt.Delta != "" {
						textBuilder.WriteString(evt.Delta)
						send("response.output_text.delta", map[string]interface{}{
							"type":          "response.output_text.delta",
							"output_index":  0,
							"content_index": 0,
							"delta":         evt.Delta,
						})
					}

				case "response.output_item.added":
					var evt struct {
						Item struct {
							Type   string `json:"type"`
							CallID string `json:"call_id"`
							Name   string `json:"name"`
							ID     string `json:"id"`
						} `json:"item"`
					}
					if json.Unmarshal(data, &evt) == nil {
						if evt.Item.Type == "function_call" {
							send("response.function_call_arguments.delta", map[string]interface{}{
								"type":         "response.function_call_arguments.delta",
								"output_index": len(outputItems),
								"call_id":      evt.Item.CallID,
								"delta":        "",
							})
						}
					}

				case "response.function_call_arguments.delta":
					var evt struct {
						Delta string `json:"delta"`
					}
					if json.Unmarshal(data, &evt) == nil {
						send("response.function_call_arguments.delta", map[string]interface{}{
							"type":  "response.function_call_arguments.delta",
							"delta": evt.Delta,
						})
					}

				case "response.completed":
					text := textBuilder.String()
					outputItems = nil
					if text != "" {
						outputItems = append(outputItems, ResponseOutputItem{
							ID:     msgID,
							Type:   "message",
							Role:   "assistant",
							Status: "completed",
							Content: []ResponseContentPart{
								{Type: "output_text", Text: text},
							},
						})
					}

					completed := &ResponsesObject{
						ID:                 respID,
						Object:             "response",
						CreatedAt:          createdAt,
						Status:             "completed",
						Model:              model,
						Output:             outputItems,
						Usage:              ResponsesUsage{InputTokens: 0, OutputTokens: len(text), TotalTokens: len(text)},
						PreviousResponseID: respReq.PreviousResponseID,
						Metadata:           respReq.Metadata,
					}
					send("response.completed", map[string]interface{}{
						"type":     "response.completed",
						"response": completed,
					})
				}
			},
		}

		err := codex.CallCodexAPI(account.AccessToken, account.UserId, req, cb)
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		h.pool.RecordSuccess(account.ID)
		h.logCallSuccess(apiKeyID, "/v1/responses", account, model, 0, textBuilder.Len(), 0)
		return
	}

	errMsg := "No available Codex accounts"
	if lastErr != nil {
		errMsg = lastErr.Error()
	}
	h.sendOpenAIError(w, 502, "server_error", errMsg)
}

// apiImportCodexAccount imports a ChatGPT Codex account using a refresh_token.
func (h *Handler) apiImportCodexAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.RefreshToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "refreshToken is required"})
		return
	}

	tr, err := codex.RefreshTokens(req.RefreshToken)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}

	accountID := ""
	email := ""
	if tr.IDToken != "" {
		accountID, _ = codex.ExtractAccountID(tr.IDToken)
	}

	expiresAt := time.Now().Unix() + int64(tr.ExpiresIn) - 60

	account := config.Account{
		ID:           config.GenerateMachineId(),
		Email:        email,
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		AuthMethod:   "codex",
		Provider:     "codex",
		Upstream:     "codex",
		UserId:       accountID,
		ExpiresAt:    expiresAt,
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	logger.Infof("[Codex] Imported account %s (email=%s accountID=%s)", account.ID, email, accountID)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": email,
		},
	})
}

// ==================== Codex OAuth Session Store ====================

type codexOAuthSession struct {
	CodeVerifier string
	CreatedAt    time.Time
}

var (
	codexOAuthSessions   = make(map[string]codexOAuthSession)
	codexOAuthSessionsMu sync.Mutex
)

func storeCodexOAuthSession(state, codeVerifier string) {
	codexOAuthSessionsMu.Lock()
	defer codexOAuthSessionsMu.Unlock()
	codexOAuthSessions[state] = codexOAuthSession{
		CodeVerifier: codeVerifier,
		CreatedAt:    time.Now(),
	}
	// Cleanup expired sessions (older than 10 minutes)
	for k, v := range codexOAuthSessions {
		if time.Since(v.CreatedAt) > 10*time.Minute {
			delete(codexOAuthSessions, k)
		}
	}
}

func popCodexOAuthSession(state string) (string, bool) {
	codexOAuthSessionsMu.Lock()
	defer codexOAuthSessionsMu.Unlock()
	sess, ok := codexOAuthSessions[state]
	if !ok {
		return "", false
	}
	delete(codexOAuthSessions, state)
	return sess.CodeVerifier, true
}

// createCodexAccountFromTokens creates a Codex account from a token response and persists it.
func createCodexAccountFromTokens(tr *codex.TokenResponse) (*config.Account, error) {
	accountID, _ := codex.ExtractAccountID(tr.IDToken)
	email, _ := codex.ExtractEmail(tr.IDToken)
	expiresAt := time.Now().Unix() + int64(tr.ExpiresIn) - 60

	account := config.Account{
		ID:           config.GenerateMachineId(),
		Email:        email,
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		AuthMethod:   "codex",
		Provider:     "codex",
		Upstream:     "codex",
		UserId:       accountID,
		ExpiresAt:    expiresAt,
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		return nil, err
	}
	return &account, nil
}

// ==================== OAuth Browser Login ====================

func (h *Handler) apiCodexOAuthStart(w http.ResponseWriter, r *http.Request) {
	pkce, err := codex.GeneratePKCE()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "PKCE generation failed"})
		return
	}

	state, err := codex.GenerateState()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "State generation failed"})
		return
	}

	storeCodexOAuthSession(state, pkce.Verifier)
	authURL := codex.BuildAuthorizationURL(pkce.Challenge, state)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"url":     authURL,
		"state":   state,
	})
}

func (h *Handler) apiCodexOAuthCallback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code        string `json:"code"`
		State       string `json:"state"`
		CallbackURL string `json:"callbackUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	code := req.Code
	state := req.State

	// If callbackUrl provided, extract code and state from it
	if code == "" && req.CallbackURL != "" {
		var err error
		code, state, err = codex.ParseCallbackURL(req.CallbackURL)
		if err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	if code == "" || state == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "code and state are required"})
		return
	}

	codeVerifier, ok := popCodexOAuthSession(state)
	if !ok {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid or expired OAuth session (state mismatch). Please try again."})
		return
	}

	tr, err := codex.ExchangeCode(code, codeVerifier)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token exchange failed: " + err.Error()})
		return
	}

	account, err := createCodexAccountFromTokens(tr)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	logger.Infof("[Codex] Imported account via OAuth %s (email=%s)", account.ID, account.Email)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

// ==================== Device Code Flow ====================

func (h *Handler) apiCodexDeviceStart(w http.ResponseWriter, r *http.Request) {
	dcr, err := codex.StartDeviceCodeFlow()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":         true,
		"userCode":        dcr.UserCode,
		"verificationUrl": codex.GetDeviceVerifyURL(),
		"deviceAuthId":    dcr.DeviceAuthID,
		"interval":        dcr.Interval,
	})
}

func (h *Handler) apiCodexDevicePoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceAuthID string `json:"deviceAuthId"`
		UserCode     string `json:"userCode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.DeviceAuthID == "" || req.UserCode == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "deviceAuthId and userCode are required"})
		return
	}

	result, err := codex.PollDeviceCode(req.DeviceAuthID, req.UserCode)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if !result.Completed {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"completed": false,
		})
		return
	}

	account, err := createCodexAccountFromTokens(result.TR)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	logger.Infof("[Codex] Imported account via device code %s (email=%s)", account.ID, account.Email)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"completed": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}
