// Package providers 描述本代理可路由到的上游服务。
// 每个提供者拥有稳定标识符（"kiro"、"codex" 等），用作 Account.Upstream 的值和路由提示。
//
// 目前仅 "kiro" 已完全接入。其他提供者仅列出以便 UI 显示为即将支持选项，
// 运维人员可以预先创建账号而不会破坏查找逻辑。
package providers

import "sort"

// Status 表示提供者请求流程的实现状态。
type Status string

const (
	StatusReady   Status = "ready"   // 真实上游支持，可提供服务。
	StatusStub    Status = "stub"    // 占位——可创建账号但请求尚未路由。
	StatusPlanned Status = "planned" // UI 中可见，尚不支持创建账号。
)

// Provider 描述代理所知的一个上游服务。
type Provider struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Status      Status `json:"status"`
	AuthHint    string `json:"authHint"`
}

var registry = []Provider{
	{
		ID:          "kiro",
		Label:       "Kiro / AWS Q",
		Description: "AWS Q Developer / Kiro IdC OAuth 连接池。当前支持 /v1/messages、/v1/chat/completions 和 /v1/responses。",
		Status:      StatusReady,
		AuthHint:    "AWS IdC OAuth（Builder ID / 社交登录）。",
	},
	{
		ID:          "codex",
		Label:       "OpenAI Codex",
		Description: "OpenAI Codex (GPT) 通过 ChatGPT OAuth。支持 GPT-5.5、GPT-5.4 及其他 Codex 模型。",
		Status:      StatusReady,
		AuthHint:    "ChatGPT OAuth 令牌（refresh_token + access_token）。",
	},
	{
		ID:          "claude-code",
		Label:       "Anthropic Claude Code",
		Description: "Claude Code OAuth 连接池。账号导入可用；请求路由保留至后续版本。",
		Status:      StatusStub,
		AuthHint:    "Anthropic OAuth 令牌。",
	},
	{
		ID:          "gemini",
		Label:       "Google Gemini CLI",
		Description: "Google Gemini CLI / AI Studio OAuth 连接池。",
		Status:      StatusPlanned,
		AuthHint:    "Google OAuth。",
	},
	{
		ID:          "grok",
		Label:       "xAI Grok Build",
		Description: "xAI Grok build OAuth 连接池。",
		Status:      StatusPlanned,
		AuthHint:    "xAI OAuth。",
	},
}

// All 返回已注册的提供者，按稳定显示顺序排列（Ready、Stub、Planned）。
func All() []Provider {
	out := make([]Provider, len(registry))
	copy(out, registry)
	rank := func(s Status) int {
		switch s {
		case StatusReady:
			return 0
		case StatusStub:
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank(out[i].Status), rank(out[j].Status)
		if ri != rj {
			return ri < rj
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// Lookup 返回指定 id 的提供者。若 id 为空或未知，返回默认提供者（"kiro"）。
func Lookup(id string) Provider {
	for _, p := range registry {
		if p.ID == id {
			return p
		}
	}
	for _, p := range registry {
		if p.ID == "kiro" {
			return p
		}
	}
	return registry[0]
}

// Normalize 将空或未知的上游 id 映射为 "kiro"，使旧版账号静默回退到唯一完全接入的提供者。
func Normalize(id string) string {
	if id == "" {
		return "kiro"
	}
	for _, p := range registry {
		if p.ID == id {
			return id
		}
	}
	return "kiro"
}

// IsReady 判断提供者当前是否能够处理请求。
func IsReady(id string) bool {
	return Lookup(id).Status == StatusReady
}
