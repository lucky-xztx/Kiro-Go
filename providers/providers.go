// Package providers describes the upstream services this proxy can route to.
// Each provider has a stable identifier ("kiro", "codex", ...) used as the
// value of Account.Upstream and as routing hints elsewhere.
//
// At the moment only "kiro" is fully wired. The others are listed so the UI
// can display them as upcoming options and operators can pre-create accounts
// without breaking lookups.
package providers

import "sort"

// Status reports whether a provider's request flow is fully implemented.
type Status string

const (
	StatusReady   Status = "ready"   // Real upstream support, can serve traffic.
	StatusStub    Status = "stub"    // Placeholder — accounts can be created but requests won't route yet.
	StatusPlanned Status = "planned" // Listed in UI for visibility, no account creation yet.
)

// Provider describes one upstream the proxy knows about.
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
		Description: "AWS Q Developer / Kiro IdC OAuth pool. Backs /v1/messages, /v1/chat/completions and /v1/responses today.",
		Status:      StatusReady,
		AuthHint:    "AWS IdC OAuth (Builder ID / Social).",
	},
	{
		ID:          "codex",
		Label:       "OpenAI Codex",
		Description: "OpenAI Codex (GPT) via ChatGPT OAuth. Supports GPT-5.5, GPT-5.4, and other Codex models.",
		Status:      StatusReady,
		AuthHint:    "ChatGPT OAuth tokens (refresh_token + access_token).",
	},
	{
		ID:          "claude-code",
		Label:       "Anthropic Claude Code",
		Description: "Claude Code OAuth pool. Account import works; request routing is reserved for a later release.",
		Status:      StatusStub,
		AuthHint:    "Anthropic OAuth tokens.",
	},
	{
		ID:          "gemini",
		Label:       "Google Gemini CLI",
		Description: "Google Gemini CLI / AI Studio OAuth pool.",
		Status:      StatusPlanned,
		AuthHint:    "Google OAuth.",
	},
	{
		ID:          "grok",
		Label:       "xAI Grok Build",
		Description: "xAI Grok build OAuth pool.",
		Status:      StatusPlanned,
		AuthHint:    "xAI OAuth.",
	},
}

// All returns the registered providers in stable display order (Ready, Stub, Planned).
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

// Lookup returns the provider with the given id, or the default ("kiro") if
// the id is empty / unknown — so code can always render something sane.
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

// Normalize maps an empty/unknown upstream id to "kiro" so legacy accounts
// silently land on the only fully-wired provider.
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

// IsReady reports whether the provider is currently able to serve requests.
func IsReady(id string) bool {
	return Lookup(id).Status == StatusReady
}
