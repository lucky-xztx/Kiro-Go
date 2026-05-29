package codex

import (
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"net/http"
	"time"
)

const usageURL = "https://chatgpt.com/backend-api/wham/usage"

// CodexUsageResponse is the JSON returned by GET /backend-api/wham/usage.
type CodexUsageResponse struct {
	PlanType            string          `json:"plan_type"`
	RateLimit           *CodexRateLimit `json:"rate_limit"`
	CodeReviewRateLimit *CodexRateLimit `json:"code_review_rate_limit"`
}

// CodexRateLimit holds rate-limit windows for a specific feature.
type CodexRateLimit struct {
	LimitReached    bool             `json:"limit_reached"`
	Allowed         bool             `json:"allowed"`
	PrimaryWindow   *CodexRateWindow `json:"primary_window"`   // ~5 hours
	SecondaryWindow *CodexRateWindow `json:"secondary_window"` // ~7 days
}

// CodexRateWindow is one time-window in a rate limit.
// used_percent is returned by the upstream as a number in [0, 100] (NOT 0-1).
type CodexRateWindow struct {
	LimitWindowSeconds int     `json:"limit_window_seconds"`
	UsedPercent        float64 `json:"used_percent"`
	ResetAt            int64   `json:"reset_at,omitempty"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds,omitempty"`
}

// GetUsage calls the ChatGPT wham/usage endpoint for the given account.
func GetUsage(accessToken, accountID string) (*CodexUsageResponse, error) {
	req, err := http.NewRequest("GET", usageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("codex usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}

	resp, err := defaultHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex usage request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io_ReadAll(resp.Body)
		return nil, fmt.Errorf("codex usage HTTP %d: %s", resp.StatusCode, string(errBody))
	}

	var usage CodexUsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&usage); err != nil {
		return nil, fmt.Errorf("codex usage decode: %w", err)
	}
	return &usage, nil
}

// RefreshCodexAccountInfo fetches usage/quota info for a Codex account and
// maps it to the unified AccountInfo struct used by the admin panel.
//
// Note: this does NOT touch Email or UserId — those fields are extracted
// from the id_token at import time. The access_token is opaque, parsing
// it as a JWT yields garbage and would overwrite the persisted id_token
// claims with each refresh. Leaving info.Email/info.UserId empty causes
// config.UpdateAccountInfo to keep the existing values.
func RefreshCodexAccountInfo(accessToken, refreshToken, accountID string) (*config.AccountInfo, error) {
	info := &config.AccountInfo{
		LastRefresh: time.Now().Unix(),
	}

	usage, err := GetUsage(accessToken, accountID)
	if err != nil {
		return info, err
	}

	info.SubscriptionType = formatPlanType(usage.PlanType)
	info.SubscriptionTitle = formatPlanTitle(usage.PlanType)

	if usage.RateLimit != nil && usage.RateLimit.PrimaryWindow != nil {
		// upstream `used_percent` is a 0-100 number; mirror that into
		// usageCurrent (so the existing 0-100 display works) and store
		// the 0.0-1.0 fractional form in usagePercent for compatibility
		// with renderers that scale it back.
		pct := usage.RateLimit.PrimaryWindow.UsedPercent
		info.UsageCurrent = pct
		info.UsageLimit = 100
		info.UsagePercent = pct / 100
	}

	return info, nil
}

func formatPlanType(plan string) string {
	switch plan {
	case "pro":
		return "pro"
	case "prolite":
		return "prolite"
	case "plus":
		return "plus"
	case "team":
		return "team"
	case "max":
		return "max"
	case "max5":
		return "max5"
	case "max20":
		return "max20"
	default:
		if plan != "" {
			return plan
		}
		return "free"
	}
}

func formatPlanTitle(plan string) string {
	switch plan {
	case "pro":
		return "Pro 20x"
	case "prolite":
		return "Pro 5x"
	case "plus":
		return "Plus"
	case "team":
		return "Team"
	case "max":
		return "Max"
	case "max5":
		return "Max 5x"
	case "max20":
		return "Max 20x"
	case "free":
		return "Free"
	default:
		if plan != "" {
			return plan
		}
		return "Free"
	}
}
