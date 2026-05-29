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
type CodexRateWindow struct {
	LimitWindowSeconds int     `json:"limit_window_seconds"`
	UsedPercent        float64 `json:"used_percent"`
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
func RefreshCodexAccountInfo(accessToken, refreshToken, accountID string) (*config.AccountInfo, error) {
	info := &config.AccountInfo{
		LastRefresh: time.Now().Unix(),
	}

	// Extract email and userId from the access token JWT.
	if email, err := ExtractEmail(accessToken); err == nil && email != "" {
		info.Email = email
	}
	if uid, err := ExtractAccountID(accessToken); err == nil && uid != "" {
		info.UserId = uid
	}

	usage, err := GetUsage(accessToken, accountID)
	if err != nil {
		return info, err
	}

	info.SubscriptionType = formatPlanType(usage.PlanType)
	info.SubscriptionTitle = formatPlanTitle(usage.PlanType)

	if usage.RateLimit != nil && usage.RateLimit.PrimaryWindow != nil {
		pct := usage.RateLimit.PrimaryWindow.UsedPercent
		info.UsagePercent = pct
		info.UsageCurrent = pct * 100
		info.UsageLimit = 100
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
