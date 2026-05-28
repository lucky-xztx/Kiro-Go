package store

import (
	"errors"
	"strings"
	"time"
)

// EnsureAdmin makes sure the `admin` user exists with role=admin. On first
// launch (or any later launch where the row is missing), it creates the user
// with the supplied password (typically the legacy config.json admin password).
//
// If a user named `admin` already exists but with the wrong role/disabled flag,
// this re-promotes them. If neither `admin` nor any admin role exists, the
// oldest existing user is promoted.
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
		// Pad short legacy passwords so bcrypt accepts them. Operators can
		// rotate via the UI afterwards.
		password = password + "kiro!!"
	}
	if u, err := CreateUser("admin", "", password, "admin"); err == nil {
		return u, nil
	} else if !errors.Is(err, ErrConflict) {
		return nil, err
	}

	// Conflict path: someone created `admin` between our checks. Re-fetch.
	if u, err := GetUserByUsername("admin"); err == nil {
		if u.Role != "admin" {
			_ = SetUserRole(u.ID, "admin")
			u.Role = "admin"
		}
		return u, nil
	}

	// Last resort: promote oldest user.
	users, err := ListUsers()
	if err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return nil, errors.New("no users available to promote")
	}
	oldest := users[len(users)-1]
	if err := SetUserRole(oldest.ID, "admin"); err != nil {
		return nil, err
	}
	oldest.Role = "admin"
	return &oldest, nil
}

// MigrateLegacyApiKeys imports a list of legacy config-level keys, attaching
// each one to the given owner. Existing rows (matched by Key value) are left
// alone. Returns the count of new rows inserted.
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
		// Skip if a key with this value is already in the table.
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
