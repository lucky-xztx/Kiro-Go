package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// ListApiKeys 返回所有已配置 API Key 条目的快照。
func ListApiKeys() []ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	out := make([]ApiKeyEntry, len(cfg.ApiKeys))
	copy(out, cfg.ApiKeys)
	return out
}

// GetApiKeyEntry 返回指定 ID 的 API Key 条目副本，未找到返回 nil。
func GetApiKeyEntry(id string) *ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			cp := cfg.ApiKeys[i]
			return &cp
		}
	}
	return nil
}

// AddApiKey 追加新的 API Key 条目。自动生成 ID 和 CreatedAt（如缺失），
// 拒绝空 Key 值和重复的已有 Key。
func AddApiKey(entry ApiKeyEntry) (ApiKeyEntry, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return ApiKeyEntry{}, errors.New("config not initialized")
	}
	entry.Key = strings.TrimSpace(entry.Key)
	if entry.Key == "" {
		return ApiKeyEntry{}, errors.New("api key value must not be empty")
	}
	for _, existing := range cfg.ApiKeys {
		if existing.Key == entry.Key {
			return ApiKeyEntry{}, errors.New("api key already exists")
		}
	}
	if entry.ID == "" {
		entry.ID = newUUID()
	}
	if entry.CreatedAt == 0 {
		entry.CreatedAt = time.Now().Unix()
	}
	cfg.ApiKeys = append(cfg.ApiKeys, entry)
	if err := saveLocked(); err != nil {
		// 回滚内存中的追加操作，避免留下不一致状态。
		cfg.ApiKeys = cfg.ApiKeys[:len(cfg.ApiKeys)-1]
		return ApiKeyEntry{}, err
	}
	return entry, nil
}

// UpdateApiKey 对已有 API Key 应用补丁更新。补丁语义：
//   - Name、Key 在补丁中非空时覆盖。
//   - Enabled、TokenLimit、CreditLimit 始终覆盖（零值也是合法值）。
//   - 计数器（TokensUsed/CreditsUsed/RequestsCount）此处不修改；
//     请使用 RecordApiKeyUsage 或 ResetApiKeyUsage。
//   - Migrated 一旦为 true 就保持不变，仅在补丁中显式设置时翻转。
func UpdateApiKey(id string, patch ApiKeyEntry) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	idx := -1
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("api key not found")
	}
	if patch.Name != "" {
		cfg.ApiKeys[idx].Name = patch.Name
	}
	if patch.Key != "" {
		newKey := strings.TrimSpace(patch.Key)
		// 拒绝与其他条目重复的 Key 值。
		for j := range cfg.ApiKeys {
			if j != idx && cfg.ApiKeys[j].Key == newKey {
				return errors.New("api key value collides with existing entry")
			}
		}
		cfg.ApiKeys[idx].Key = newKey
	}
	cfg.ApiKeys[idx].Enabled = patch.Enabled
	cfg.ApiKeys[idx].TokenLimit = patch.TokenLimit
	cfg.ApiKeys[idx].CreditLimit = patch.CreditLimit
	if patch.Migrated {
		cfg.ApiKeys[idx].Migrated = true
	}
	return saveLocked()
}

// DeleteApiKey 删除指定 ID 的 API Key 条目。即使 ID 不存在也返回 nil（幂等），
// 与已有的 DeleteAccount 风格一致。
func DeleteApiKey(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i, e := range cfg.ApiKeys {
		if e.ID == id {
			cfg.ApiKeys = append(cfg.ApiKeys[:i], cfg.ApiKeys[i+1:]...)
			return saveLocked()
		}
	}
	return nil
}

// FindApiKeyByValue 返回 Key 值匹配的条目副本，未找到返回 nil。O(n) 线性扫描。
func FindApiKeyByValue(key string) *ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || key == "" {
		return nil
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].Key == key {
			cp := cfg.ApiKeys[i]
			return &cp
		}
	}
	return nil
}

// HasApiKeys 当至少配置了一个 API Key 条目时返回 true。
func HasApiKeys() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return len(cfg.ApiKeys) > 0
}

// RecordApiKeyUsage 原子地累加 Token 和额度计数器，更新 LastUsedAt 和 RequestsCount，并持久化。
func RecordApiKeyUsage(id string, tokens int64, credits float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			if tokens > 0 {
				cfg.ApiKeys[i].TokensUsed += tokens
			}
			if credits > 0 {
				cfg.ApiKeys[i].CreditsUsed += credits
			}
			cfg.ApiKeys[i].RequestsCount++
			cfg.ApiKeys[i].LastUsedAt = time.Now().Unix()
			return saveLocked()
		}
	}
	return errors.New("api key not found")
}

// ResetApiKeyUsage 清零 TokensUsed/CreditsUsed/RequestsCount。
// 保留 LastUsedAt 以便运维人员查看 Key 最后使用时间。
func ResetApiKeyUsage(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			cfg.ApiKeys[i].TokensUsed = 0
			cfg.ApiKeys[i].CreditsUsed = 0
			cfg.ApiKeys[i].RequestsCount = 0
			return saveLocked()
		}
	}
	return errors.New("api key not found")
}

// GenerateApiKeyValue 生成新的 32 字节随机十六进制 API Key，前缀为 "sk-"。
func GenerateApiKeyValue() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return "sk-" + hex.EncodeToString(buf)
}

// MaskApiKey 生成用于展示的脱敏版本：保留前 6 位和后 4 位字符，
// 中间用 "****" 替换。空字符串返回 ""，过短的字符串原样返回。
func MaskApiKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 10 {
		return key
	}
	return key[:6] + "****" + key[len(key)-4:]
}

// ApiKeyOverLimit 返回 (overToken, overCredit) 是否超限。值为 0 的限制被忽略。
// 此函数不加锁，调用方应传入已复制的条目。
func ApiKeyOverLimit(e ApiKeyEntry) (overToken bool, overCredit bool) {
	if e.TokenLimit > 0 && e.TokensUsed >= e.TokenLimit {
		overToken = true
	}
	if e.CreditLimit > 0 && e.CreditsUsed >= e.CreditLimit {
		overCredit = true
	}
	return
}

// GetLegacyApiKeysForMigration 返回 ApiKeys 的快照，供 SQLite store 在首次启动时迁移。
// 迁移完成后返回 nil。
func GetLegacyApiKeysForMigration() []ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || len(cfg.ApiKeys) == 0 {
		return nil
	}
	out := make([]ApiKeyEntry, len(cfg.ApiKeys))
	copy(out, cfg.ApiKeys)
	return out
}

// ClearLegacyApiKeys 清空配置中的 ApiKeys 切片并持久化。
// 在迁移到 SQLite store 成功后调用一次，避免 JSON 文件中残留重复数据。
func ClearLegacyApiKeys() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return nil
	}
	cfg.ApiKeys = nil
	cfg.ApiKey = ""
	return saveLocked()
}
