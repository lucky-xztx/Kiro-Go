package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

const (
	baseURL     = "https://chatgpt.com/backend-api/codex"
	responsesEP = "/responses"
	userAgent   = "codex_cli_rs/0.133.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9"
)

var httpClientStore atomic.Pointer[http.Client]

func init() {
	SetProxy("")
}

// SetProxy reconfigures the codex HTTP client to use the given outbound proxy URL.
// An empty string falls back to HTTPS_PROXY/HTTP_PROXY environment variables.
func SetProxy(proxyURL string) {
	t := &http.Transport{
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	if strings.TrimSpace(proxyURL) != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			t.Proxy = http.ProxyURL(u)
			t.ForceAttemptHTTP2 = false
		}
	} else {
		t.Proxy = http.ProxyFromEnvironment
	}
	httpClientStore.Store(&http.Client{
		Timeout:   5 * time.Minute,
		Transport: t,
	})
}

func defaultHTTPClient() *http.Client {
	if c := httpClientStore.Load(); c != nil {
		return c
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

// CodexRequest is the body sent to chatgpt.com/backend-api/codex/responses.
type CodexRequest struct {
	Instructions      string          `json:"instructions,omitempty"`
	Stream            bool            `json:"stream"`
	Model             string          `json:"model"`
	Input             json.RawMessage `json:"input"`
	Reasoning         *CodexReasoning `json:"reasoning,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	Include           []string        `json:"include,omitempty"`
	Tools             json.RawMessage `json:"tools,omitempty"`
	Store             *bool           `json:"store,omitempty"`
	PromptCacheKey    string          `json:"prompt_cache_key,omitempty"`
}

type CodexReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// StreamEvent represents a single SSE event from the Codex responses API.
type StreamEvent struct {
	Type  string          `json:"type"`
	Delta string          `json:"delta,omitempty"`
	Raw   json.RawMessage `json:"-"`
}

// StreamCallback receives parsed events from the Codex SSE stream.
type StreamCallback struct {
	// OnEvent is called for every SSE event with the full parsed JSON object.
	OnEvent func(eventType string, data json.RawMessage)
	// OnHTTPStatus, if set, is called once with the raw upstream HTTP status
	// code as soon as the response headers arrive (before the 200 check).
	// Useful for surfacing the real chatgpt.com status in diagnostics.
	OnHTTPStatus func(status int)
}

// CallCodexAPI sends a request to the Codex Responses API and processes the
// streaming (or non-streaming) response via the provided callback.
func CallCodexAPI(accessToken, accountID string, req *CodexRequest, cb *StreamCallback) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("codex marshal request: %w", err)
	}

	endpoint := baseURL + responsesEP
	httpReq, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("codex create request: %w", err)
	}

	applyCodexHeaders(httpReq, accessToken, accountID, req.Stream)

	resp, err := defaultHTTPClient().Do(httpReq)
	if err != nil {
		return fmt.Errorf("codex request failed: %w", err)
	}
	defer resp.Body.Close()

	// Surface the real upstream HTTP status before any short-circuit so that
	// diagnostics can prove the request actually reached chatgpt.com.
	if cb != nil && cb.OnHTTPStatus != nil {
		cb.OnHTTPStatus(resp.StatusCode)
	}

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("codex HTTP %d: %s", resp.StatusCode, string(errBody))
	}

	if req.Stream {
		return parseSSEStream(resp.Body, cb)
	}

	// Non-streaming: read the full response and pass as a single event.
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("codex read response: %w", err)
	}
	if cb != nil && cb.OnEvent != nil {
		cb.OnEvent("response.completed", respBody)
	}
	return nil
}

func applyCodexHeaders(req *http.Request, accessToken, accountID string, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("Connection", "Keep-Alive")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Originator", "codex_cli_rs")
	if accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
}

// parseSSEStream reads SSE text/event-stream from the Codex API.
// Each "data: ..." line is forwarded to the callback.
func parseSSEStream(body io.Reader, cb *StreamCallback) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 50*1024*1024) // 50MB max line

	for scanner.Scan() {
		line := scanner.Text()

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			return nil
		}

		// Extract event type from the JSON
		var evt struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			continue
		}

		if cb != nil && cb.OnEvent != nil {
			cb.OnEvent(evt.Type, json.RawMessage(data))
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("codex SSE read error: %w", err)
	}
	return nil
}
