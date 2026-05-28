package codex

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ConvertOpenAIToCodex translates an OpenAI Chat Completions request into a
// Codex Responses API request body. The caller must supply the raw OpenAI
// request bytes (already parsed as the proxy's internal OpenAIRequest).
func ConvertOpenAIToCodex(messages []OpenAIChatMessage, model string, stream bool, tools json.RawMessage, reasoningEffort string) *CodexRequest {
	var inputItems []interface{}

	for _, msg := range messages {
		switch msg.Role {
		case "system":
			inputItems = append(inputItems, map[string]interface{}{
				"type": "message",
				"role": "developer",
				"content": []interface{}{
					map[string]interface{}{
						"type": "input_text",
						"text": extractText(msg.Content),
					},
				},
			})

		case "user":
			parts := buildCodexUserContent(msg.Content)
			inputItems = append(inputItems, map[string]interface{}{
				"type":    "message",
				"role":    "user",
				"content": parts,
			})

		case "assistant":
			// If tool_calls present, emit them as top-level function_call items.
			if len(msg.ToolCalls) > 0 {
				// Emit the text part first if present
				if text := extractText(msg.Content); text != "" {
					inputItems = append(inputItems, map[string]interface{}{
						"type":    "message",
						"role":    "assistant",
						"content": []interface{}{map[string]interface{}{"type": "output_text", "text": text}},
					})
				}
				for _, tc := range msg.ToolCalls {
					inputItems = append(inputItems, map[string]interface{}{
						"type":      "function_call",
						"call_id":   tc.ID,
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					})
				}
			} else {
				text := extractText(msg.Content)
				inputItems = append(inputItems, map[string]interface{}{
					"type":    "message",
					"role":    "assistant",
					"content": []interface{}{map[string]interface{}{"type": "output_text", "text": text}},
				})
			}

		case "tool":
			inputItems = append(inputItems, map[string]interface{}{
				"type":    "function_call_output",
				"call_id": msg.ToolCallID,
				"output":  extractText(msg.Content),
			})
		}
	}

	inputJSON, _ := json.Marshal(inputItems)

	req := &CodexRequest{
		Instructions: "",
		Stream:       stream,
		Model:        model,
		Input:        inputJSON,
	}

	if reasoningEffort != "" {
		req.Reasoning = &CodexReasoning{
			Effort:  reasoningEffort,
			Summary: "auto",
		}
	}

	if len(tools) > 0 {
		req.Tools = tools
	}

	// Include encrypted reasoning content
	req.Include = []string{"reasoning.encrypted_content"}

	return req
}

// OpenAIChatMessage mirrors the proxy's internal OpenAIMessage for convenience.
type OpenAIChatMessage struct {
	Role       string           `json:"role"`
	Content    interface{}      `json:"content"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type OpenAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// CodexResponseEvent represents relevant SSE events from the Codex Responses API.
type CodexResponseEvent struct {
	Type  string          `json:"type"`
	Delta string          `json:"delta,omitempty"`
	Raw   json.RawMessage `json:"-"`
}

// ParseCodexStreamEvent extracts useful data from a Codex SSE event.
type StreamEventParser struct {
	ResponseID   string
	OutputIndex  int
	ToolCalls    []ToolCallState
	TextBuilder  strings.Builder
	Done         bool
	FinishReason string
}

type ToolCallState struct {
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Index     int
}

// ProcessEvent updates state from a single SSE event data.
func (p *StreamEventParser) ProcessEvent(eventType string, raw json.RawMessage) {
	switch eventType {
	case "response.created":
		var evt struct {
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		if json.Unmarshal(raw, &evt) == nil {
			p.ResponseID = evt.Response.ID
		}

	case "response.output_text.delta":
		var evt struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(raw, &evt) == nil {
			p.TextBuilder.WriteString(evt.Delta)
		}

	case "response.reasoning_summary_text.delta":
		// Reasoning content — we'll append it to the text as well for now
		var evt struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(raw, &evt) == nil {
			p.TextBuilder.WriteString(evt.Delta)
		}

	case "response.output_item.added":
		var evt struct {
			Item struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
				Index  int    `json:"index"`
			} `json:"item"`
		}
		if json.Unmarshal(raw, &evt) == nil && evt.Item.Type == "function_call" {
			p.ToolCalls = append(p.ToolCalls, ToolCallState{
				CallID: evt.Item.CallID,
				Name:   evt.Item.Name,
				Index:  len(p.ToolCalls),
			})
		}

	case "response.function_call_arguments.delta":
		var evt struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(raw, &evt) == nil && len(p.ToolCalls) > 0 {
			last := &p.ToolCalls[len(p.ToolCalls)-1]
			last.Arguments += evt.Delta
		}

	case "response.function_call_arguments.done":
		var evt struct {
			Arguments string `json:"arguments"`
		}
		if json.Unmarshal(raw, &evt) == nil && len(p.ToolCalls) > 0 {
			last := &p.ToolCalls[len(p.ToolCalls)-1]
			if evt.Arguments != "" {
				last.Arguments = evt.Arguments
			}
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
		if json.Unmarshal(raw, &evt) == nil && evt.Item.Type == "function_call" {
			// Update or append
			found := false
			for i := range p.ToolCalls {
				if p.ToolCalls[i].CallID == evt.Item.CallID {
					p.ToolCalls[i].Arguments = evt.Item.Arguments
					found = true
					break
				}
			}
			if !found && evt.Item.CallID != "" {
				p.ToolCalls = append(p.ToolCalls, ToolCallState{
					CallID:    evt.Item.CallID,
					Name:      evt.Item.Name,
					Arguments: evt.Item.Arguments,
					Index:     len(p.ToolCalls),
				})
			}
		}

	case "response.completed":
		p.Done = true
		var evt struct {
			Response struct {
				Status string `json:"status"`
				Output []struct {
					Type    string `json:"type"`
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content,omitempty"`
					CallID    string `json:"call_id,omitempty"`
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				} `json:"output,omitempty"`
			} `json:"response"`
		}
		if json.Unmarshal(raw, &evt) == nil {
			if len(p.ToolCalls) > 0 {
				p.FinishReason = "tool_calls"
			} else {
				p.FinishReason = "stop"
			}
		}

	case "error":
		p.Done = true
		p.FinishReason = "error"
	}
}

// ExtractText extracts plain text from a content field (string or array).
func extractText(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		var sb strings.Builder
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				if t, _ := m["text"].(string); t != "" {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	default:
		return fmt.Sprintf("%v", content)
	}
}

func buildCodexUserContent(content interface{}) []interface{} {
	switch v := content.(type) {
	case string:
		return []interface{}{
			map[string]interface{}{
				"type": "input_text",
				"text": v,
			},
		}
	case []interface{}:
		var parts []interface{}
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				partType, _ := m["type"].(string)
				switch partType {
				case "text":
					text, _ := m["text"].(string)
					parts = append(parts, map[string]interface{}{
						"type": "input_text",
						"text": text,
					})
				case "image_url":
					imgURL, _ := m["image_url"].(map[string]interface{})
					if imgURL != nil {
						url, _ := imgURL["url"].(string)
						parts = append(parts, map[string]interface{}{
							"type":      "input_image",
							"image_url": url,
						})
					}
				}
			}
		}
		if len(parts) == 0 {
			return []interface{}{
				map[string]interface{}{"type": "input_text", "text": extractText(content)},
			}
		}
		return parts
	default:
		return []interface{}{
			map[string]interface{}{
				"type": "input_text",
				"text": extractText(content),
			},
		}
	}
}
