package codex

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// modelsURL 是 ChatGPT 后端端点，列出给定账号/套餐可用的模型。
// 与 Codex CLI 自身的模型发现逻辑一致。
const modelsURL = "https://chatgpt.com/backend-api/codex/models"

// clientVersion 作为 ?client_version= 查询参数发送。
// 必须与 User-Agent 中声明的 CLI 版本一致，否则上游会返回未知客户端错误而非真实模型集。
const clientVersion = "0.133.0"

// CodexModel 是 GET /backend-api/codex/models 返回的单个模型条目。
type CodexModel struct {
	Slug            string   `json:"slug"`
	DisplayName     string   `json:"display_name"`
	Description     string   `json:"description"`
	InputModalities []string `json:"input_modalities"`
	ContextWindow   int      `json:"context_window"`
	SupportedInAPI  bool     `json:"supported_in_api"`
}

// FetchCodexModels 调用 ChatGPT 模型端点，返回给定账号可用的实时模型列表。
// 无硬编码回退——若上游调用失败，调用方收到错误并自行决定处理方式。
func FetchCodexModels(accessToken, accountID string) ([]CodexModel, error) {
	req, err := http.NewRequest("GET", modelsURL+"?client_version="+clientVersion, nil)
	if err != nil {
		return nil, fmt.Errorf("codex models request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	// Originator identifies the calling CLI. The Codex backend requires it on the
	// models endpoint (the responses endpoint sets it too via applyCodexHeaders);
	// omitting it makes chatgpt.com reject the request even with a valid token.
	// Matches the canonical Codex CLI behavior (see CLIProxyAPI fetch_codex_models).
	req.Header.Set("Originator", "codex_cli_rs")
	if accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}

	resp, err := defaultHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex models request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io_ReadAll(resp.Body)
		return nil, fmt.Errorf("codex models HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Models []CodexModel `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("codex models decode: %w", err)
	}

	out := result.Models[:0:0]
	for _, m := range result.Models {
		if m.Slug == "" {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}
