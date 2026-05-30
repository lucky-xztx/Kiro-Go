package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

// kiroQAPIBase 是 AWS Q Developer 端点，拥有用户级别的超额开关。
// 与项目中其他地方使用的 kiroRestAPIBase（CodeWhisperer）不同。
const kiroQAPIBase = "https://q.us-east-1.amazonaws.com"

// OverageSnapshot 捕获账号的上游超额使用状态快照。
type OverageSnapshot struct {
	Status            string  `json:"status"`            // "ENABLED" | "DISABLED" | "UNKNOWN"
	Capability        string  `json:"capability"`        // "OVERAGE_CAPABLE" | ...
	SubscriptionTitle string  `json:"subscriptionTitle"` // e.g. "KIRO PRO+"
	OverageCap        float64 `json:"overageCap"`        // USD upper bound
	OverageRate       float64 `json:"overageRate"`       // per-invocation USD
	CurrentOverages   float64 `json:"currentOverages"`   // accumulated overage USD
	CheckedAt         int64   `json:"checkedAt"`         // Unix seconds
}

// upstreamOverageResponse 映射 /getUsageLimits 响应中与超额开关相关的字段。
type upstreamOverageResponse struct {
	OverageConfiguration *struct {
		OverageStatus string `json:"overageStatus"`
	} `json:"overageConfiguration"`
	SubscriptionInfo *struct {
		OverageCapability string `json:"overageCapability"`
		SubscriptionTitle string `json:"subscriptionTitle"`
	} `json:"subscriptionInfo"`
	UsageBreakdownList []struct {
		ResourceType    string  `json:"resourceType"`
		OverageCap      float64 `json:"overageCap"`
		OverageRate     float64 `json:"overageRate"`
		CurrentOverages float64 `json:"currentOverages"`
	} `json:"usageBreakdownList"`
}

// FetchOverageStatus 调用 AWS Q GET /getUsageLimits，提取超额开关状态和订阅元数据。
func FetchOverageStatus(account *config.Account) (*OverageSnapshot, error) {
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}

	rawURL := kiroQAPIBase + "/getUsageLimits?origin=AI_EDITOR&resourceType=AGENTIC_REQUEST&isEmailRequired=true"
	if profileArn := strings.TrimSpace(account.ProfileArn); profileArn != "" {
		rawURL += "&profileArn=" + neturl.QueryEscape(profileArn)
	}

	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	setKiroHeaders(req, account)

	resp, err := GetRestClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var parsed upstreamOverageResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode getUsageLimits: %w", err)
	}

	snap := &OverageSnapshot{
		Status:    "UNKNOWN",
		CheckedAt: time.Now().Unix(),
	}
	if parsed.OverageConfiguration != nil && parsed.OverageConfiguration.OverageStatus != "" {
		snap.Status = strings.ToUpper(parsed.OverageConfiguration.OverageStatus)
	}
	if parsed.SubscriptionInfo != nil {
		snap.Capability = parsed.SubscriptionInfo.OverageCapability
		snap.SubscriptionTitle = parsed.SubscriptionInfo.SubscriptionTitle
	}
	for _, bd := range parsed.UsageBreakdownList {
		if bd.OverageCap > 0 || bd.OverageRate > 0 || bd.CurrentOverages > 0 {
			snap.OverageCap = bd.OverageCap
			snap.OverageRate = bd.OverageRate
			snap.CurrentOverages = bd.CurrentOverages
			break
		}
	}
	return snap, nil
}

// SetOverageStatus 调用 AWS Q POST /setUserPreference 切换用户级别的超额开关，
// 然后重新拉取快照以保证缓存一致性。
//
// enabled=true  → overageStatus="ENABLED"
// enabled=false → overageStatus="DISABLED"
func SetOverageStatus(account *config.Account, enabled bool) (*OverageSnapshot, error) {
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}

	profileArn, err := ResolveProfileArn(account)
	if err != nil {
		return nil, fmt.Errorf("resolve profileArn: %w", err)
	}

	status := "DISABLED"
	if enabled {
		status = "ENABLED"
	}
	payload := map[string]interface{}{
		"overageConfiguration": map[string]string{
			"overageStatus": status,
		},
		"profileArn": profileArn,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", kiroQAPIBase+"/setUserPreference", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	setKiroHeaders(req, account)
	req.Header.Set("Content-Type", "application/json")

	resp, err := GetRestClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("setUserPreference HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	logger.Infof("[Overage] account=%s flipped overageStatus=%s upstream", account.Email, status)

	// 尽力重新拉取，确保缓存字段（cap/rate/current）保持准确。
	snap, fetchErr := FetchOverageStatus(account)
	if fetchErr != nil {
		// POST 已成功，返回合成的快照即可。
		logger.Warnf("[Overage] re-fetch after switch failed for %s: %v", account.Email, fetchErr)
		return &OverageSnapshot{
			Status:    status,
			CheckedAt: time.Now().Unix(),
		}, nil
	}
	// AWS 偶有延迟，强制使用刚设置的值。
	snap.Status = status
	return snap, nil
}

// PersistOverageSnapshot 将快照写回 config.json 中的账号配置。
// 返回持久化错误（如有），由调用方决定是否展示。
func PersistOverageSnapshot(accountID string, snap *OverageSnapshot) error {
	if snap == nil {
		return nil
	}
	return config.UpdateAccountOverageStatus(
		accountID,
		snap.Status,
		snap.Capability,
		snap.OverageCap,
		snap.OverageRate,
		snap.CurrentOverages,
		snap.CheckedAt,
	)
}
