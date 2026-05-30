package store

import (
	"errors"
	"strings"
	"time"
)

// EnsureAdmin 确保 `admin` 用户存在且角色为 admin。
// 首次启动（或后续 admin 行缺失时），使用提供的密码创建该用户
// （通常是旧版 config.json 中的管理员密码）。
//
// 如果名为 `admin` 的用户已存在但角色/启用状态不正确，会重新提升。
// 如果既没有 `admin` 用户也没有任何 admin 角色，则提升最早创建的用户。
func EnsureAdmin(defaultPassword string) (*User, error) {
	if u, err := GetUserByUsername("admin"); err == nil {
		if u.Role != "admin" {
			_ = SetUserRole(u.ID, "admin")
			u.Role = "admin"
		}
		if !u.Enabled {
			_ = SetUserEnabled(u.ID, true)
			u.Enabled = true
		}
		return u, nil
	}

	password := strings.TrimSpace(defaultPassword)
	if password == "" {
		password = "admin123"
	}
	if len(password) < 6 {
		// 短密码补位以满足 bcrypt 最小长度要求，操作员可通过 UI 后续修改
		password = password + "kiro!!"
	}
	if u, err := CreateUser("admin", "", password, "admin"); err == nil {
		return u, nil
	} else if !errors.Is(err, ErrConflict) {
		return nil, err
	}

	// 冲突路径：在检查之间有人创建了 `admin`，重新获取
	if u, err := GetUserByUsername("admin"); err == nil {
		if u.Role != "admin" {
			_ = SetUserRole(u.ID, "admin")
			u.Role = "admin"
		}
		return u, nil
	}

	// 最后手段：提升最早创建的用户
	users, err := ListUsers()
	if err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return nil, errors.New("没有可提升的用户")
	}
	oldest := users[len(users)-1]
	if err := SetUserRole(oldest.ID, "admin"); err != nil {
		return nil, err
	}
	oldest.Role = "admin"
	return &oldest, nil
}

// LegacyApiKey 表示旧版 config 级别的 API Key，用于迁移。
type LegacyApiKey struct {
	ID            string
	Name          string
	Key           string
	Enabled       bool
	CreatedAt     int64
	LastUsedAt    int64
	TokenLimit    int64
	CreditLimit   float64
	TokensUsed    int64
	CreditsUsed   float64
	RequestsCount int64
}

// MigrateLegacyApiKeys 导入旧版 config 级别的 Key 列表，将其关联到指定用户。
// 已存在的 Key（按值匹配）会被跳过。返回新插入的行数。
func MigrateLegacyApiKeys(ownerID string, legacy []LegacyApiKey) (int, error) {
	if ownerID == "" || len(legacy) == 0 {
		return 0, nil
	}
	inserted := 0
	for _, l := range legacy {
		key := strings.TrimSpace(l.Key)
		if key == "" {
			continue
		}
		// 跳过已存在于数据库中的 Key
		if _, err := FindApiKeyByValue(key); err == nil {
			continue
		}
		id := l.ID
		if id == "" {
			id = newID()
		}
		createdAt := l.CreatedAt
		if createdAt == 0 {
			createdAt = time.Now().Unix()
		}
		_, err := db.Exec(
			`INSERT INTO user_api_keys(id, user_id, name, key, enabled, created_at, last_used_at,
				token_limit, credit_limit, tokens_used, credits_used, requests_count)
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, ownerID, nullableString(strings.TrimSpace(l.Name)), key,
			boolToInt(l.Enabled), createdAt, l.LastUsedAt,
			l.TokenLimit, l.CreditLimit, l.TokensUsed, l.CreditsUsed, l.RequestsCount,
		)
		if err != nil {
			if isUniqueErr(err) {
				continue
			}
			return inserted, err
		}
		inserted++
	}
	return inserted, nil
}
