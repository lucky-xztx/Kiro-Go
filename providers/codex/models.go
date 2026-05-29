package codex

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// modelsURL is the ChatGPT backend endpoint that lists the models a given
// account/plan can actually use. Mirrors the Codex CLI's own model discovery.
const modelsURL = "https://chatgpt.com/backend-api/codex/models"

// clientVersion is sent as the ?client_version= query param. It must match the
// CLI version advertised in the User-Agent so the upstream returns the plan's
// real model set rather than rejecting an unknown client.
const clientVersion = "0.118.0"

// CodexModel is a single entry returned by GET /backend-api/codex/models.
type CodexModel struct {
	Slug            string   `json:"slug"`
	DisplayName     string   `json:"display_name"`
	Description     string   `json:"description"`
	InputModalities []string `json:"input_modalities"`
	ContextWindow   int      `json:"context_window"`
	SupportedInAPI  bool     `json:"supported_in_api"`
}

// FetchCodexModels calls the ChatGPT models endpoint and returns the live list
// of models available to the given account. No hardcoded fallback — if the
// upstream call fails the caller gets the error and decides what to do.
func FetchCodexModels(accessToken, accountID string) ([]CodexModel, error) {
	req, err := http.NewRequest("GET", modelsURL+"?client_version="+clientVersion, nil)
	if err != nil {
		return nil, fmt.Errorf("codex models request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
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
