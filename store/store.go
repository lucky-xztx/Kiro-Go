// Package store provides SQLite-backed persistence for user accounts,
// per-user API keys, and login sessions.
//
// The Kiro upstream account pool and global server settings still live in
// data/config.json — this package only owns user-facing identity data.
//
// Driver: modernc.org/sqlite (pure Go, no CGO).
package store

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

// User represents a registered user account.
type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	Email        string `json:"email,omitempty"`
	PasswordHash string `json:"-"`
	Role         string `json:"role"` // "admin" or "user"
	Enabled      bool   `json:"enabled"`
	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
}

// UserApiKey represents one API key issued to a user.
type UserApiKey struct {
	ID            string  `json:"id"`
	UserID        string  `json:"userId"`
	Name          string  `json:"name,omitempty"`
	Key           string  `json:"key"`
	Enabled       bool    `json:"enabled"`
	CreatedAt     int64   `json:"createdAt"`
	LastUsedAt    int64   `json:"lastUsedAt,omitempty"`
	TokenLimit    int64   `json:"tokenLimit,omitempty"`
	CreditLimit   float64 `json:"creditLimit,omitempty"`
	TokensUsed    int64   `json:"tokensUsed"`
	CreditsUsed   float64 `json:"creditsUsed"`
	RequestsCount int64   `json:"requestsCount"`
}

// Session is a server-side session record bound to a cookie value.
type Session struct {
	Token     string
	UserID    string
	CreatedAt int64
	ExpiresAt int64
}

var (
	db   *sql.DB
	once sync.Once

	// ErrNotFound is returned when a row lookup misses.
	ErrNotFound = errors.New("not found")
	// ErrConflict is returned for unique-constraint violations the caller can
	// translate into "already exists" messages.
	ErrConflict = errors.New("conflict")
)

const (
	sessionTTL = 30 * 24 * time.Hour // 30 days
)

// Init opens (or creates) the SQLite database at path and applies the schema.
// Safe to call multiple times — only the first invocation does the work.
func Init(path string) error {
	var initErr error
	once.Do(func() {
		// _pragma options ensure WAL + foreign keys at connection time.
		dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", path)
		conn, err := sql.Open("sqlite", dsn)
		if err != nil {
			initErr = fmt.Errorf("open sqlite: %w", err)
			return
		}
		// SQLite handles concurrency at the file level; a small pool is plenty.
		conn.SetMaxOpenConns(4)
		conn.SetMaxIdleConns(2)
		conn.SetConnMaxIdleTime(5 * time.Minute)

		if err := conn.Ping(); err != nil {
			initErr = fmt.Errorf("ping sqlite: %w", err)
			return
		}
		if err := applySchema(conn); err != nil {
			initErr = fmt.Errorf("apply schema: %w", err)
			return
		}
		db = conn
	})
	return initErr
}

// DB returns the underlying *sql.DB. Mostly useful for tests.
func DB() *sql.DB { return db }

func applySchema(conn *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL UNIQUE,
			email TEXT,
			password_hash TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'user',
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_users_username ON users(username)`,

		`CREATE TABLE IF NOT EXISTS user_api_keys (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			name TEXT,
			key TEXT NOT NULL UNIQUE,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL,
			last_used_at INTEGER,
			token_limit INTEGER NOT NULL DEFAULT 0,
			credit_limit REAL NOT NULL DEFAULT 0,
			tokens_used INTEGER NOT NULL DEFAULT 0,
			credits_used REAL NOT NULL DEFAULT 0,
			requests_count INTEGER NOT NULL DEFAULT 0,
			FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_user_api_keys_user ON user_api_keys(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_user_api_keys_key ON user_api_keys(key)`,

		`CREATE TABLE IF NOT EXISTS sessions (
			token TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id)`,

		`CREATE TABLE IF NOT EXISTS request_logs (
			id TEXT PRIMARY KEY,
			created_at INTEGER NOT NULL,
			api_key_id TEXT,
			user_id TEXT,
			account_id TEXT,
			provider TEXT,
			model TEXT,
			path TEXT,
			status INTEGER,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			credits REAL NOT NULL DEFAULT 0,
			latency_ms INTEGER NOT NULL DEFAULT 0,
			error TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_created ON request_logs(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_api_key ON request_logs(api_key_id)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_user ON request_logs(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_account ON request_logs(account_id)`,

		`CREATE TABLE IF NOT EXISTS model_aliases (
			alias TEXT PRIMARY KEY,
			target TEXT NOT NULL,
			provider TEXT,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,

		`CREATE TABLE IF NOT EXISTS account_health (
			account_id TEXT PRIMARY KEY,
			status TEXT NOT NULL,
			last_check_at INTEGER NOT NULL,
			last_ok_at INTEGER,
			fail_streak INTEGER NOT NULL DEFAULT 0,
			last_error TEXT,
			auto_disabled INTEGER NOT NULL DEFAULT 0
		)`,
	}
	for _, s := range stmts {
		if _, err := conn.Exec(s); err != nil {
			return fmt.Errorf("exec %q: %w", firstLine(s), err)
		}
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

// ====================== Users ======================

// CreateUser inserts a new user with a bcrypt-hashed password.
// Returns ErrConflict on duplicate username.
func CreateUser(username, email, password, role string) (*User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, errors.New("username is required")
	}
	if len(password) < 6 {
		return nil, errors.New("password must be at least 6 characters")
	}
	if role != "admin" && role != "user" {
		role = "user"
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	now := time.Now().Unix()
	u := &User{
		ID:           newID(),
		Username:     username,
		Email:        strings.TrimSpace(email),
		PasswordHash: string(hash),
		Role:         role,
		Enabled:      true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	_, err = db.Exec(
		`INSERT INTO users(id, username, email, password_hash, role, enabled, created_at, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Username, nullableString(u.Email), u.PasswordHash, u.Role, boolToInt(u.Enabled), u.CreatedAt, u.UpdatedAt,
	)
	if err != nil {
		if isUniqueErr(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	return u, nil
}

// GetUserByUsername returns the user (case-insensitive match) or ErrNotFound.
func GetUserByUsername(username string) (*User, error) {
	if db == nil {
		return nil, ErrNotFound
	}
	row := db.QueryRow(
		`SELECT id, username, IFNULL(email,''), password_hash, role, enabled, created_at, updated_at
		 FROM users WHERE LOWER(username) = LOWER(?)`,
		strings.TrimSpace(username),
	)
	return scanUser(row)
}

// GetUserByID returns the user with the given ID or ErrNotFound.
func GetUserByID(id string) (*User, error) {
	if db == nil {
		return nil, ErrNotFound
	}
	row := db.QueryRow(
		`SELECT id, username, IFNULL(email,''), password_hash, role, enabled, created_at, updated_at
		 FROM users WHERE id = ?`,
		id,
	)
	return scanUser(row)
}

// ListUsers returns all users, newest first.
func ListUsers() ([]User, error) {
	rows, err := db.Query(
		`SELECT id, username, IFNULL(email,''), password_hash, role, enabled, created_at, updated_at
		 FROM users ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// UserCount returns the number of registered users.
func UserCount() (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// UpdateUserPassword replaces the bcrypt hash for the given user ID.
func UpdateUserPassword(id, newPassword string) error {
	if len(newPassword) < 6 {
		return errors.New("password must be at least 6 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	res, err := db.Exec(
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		string(hash), time.Now().Unix(), id,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetUserEnabled toggles a user account.
func SetUserEnabled(id string, enabled bool) error {
	res, err := db.Exec(
		`UPDATE users SET enabled = ?, updated_at = ? WHERE id = ?`,
		boolToInt(enabled), time.Now().Unix(), id,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetUserRole changes admin/user role.
func SetUserRole(id, role string) error {
	if role != "admin" && role != "user" {
		return errors.New("role must be 'admin' or 'user'")
	}
	res, err := db.Exec(
		`UPDATE users SET role = ?, updated_at = ? WHERE id = ?`,
		role, time.Now().Unix(), id,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser removes the user and (via FK cascade) all their api keys + sessions.
func DeleteUser(id string) error {
	_, err := db.Exec(`DELETE FROM users WHERE id = ?`, id)
	return err
}

// VerifyPassword returns the user when (username, password) matches and the
// account is enabled. Constant-time bcrypt comparison.
func VerifyPassword(username, password string) (*User, error) {
	u, err := GetUserByUsername(username)
	if err != nil {
		return nil, err
	}
	if !u.Enabled {
		return nil, errors.New("account disabled")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return nil, errors.New("invalid credentials")
	}
	return u, nil
}

// ====================== API Keys ======================

// CreateApiKey issues a new key for the given user.
// If keyValue is empty, a fresh sk-<hex> token is generated.
func CreateApiKey(userID, name, keyValue string, tokenLimit int64, creditLimit float64) (*UserApiKey, error) {
	if userID == "" {
		return nil, errors.New("userID is required")
	}
	if keyValue == "" {
		keyValue = GenerateApiKeyValue()
	}
	now := time.Now().Unix()
	k := &UserApiKey{
		ID:          newID(),
		UserID:      userID,
		Name:        strings.TrimSpace(name),
		Key:         keyValue,
		Enabled:     true,
		CreatedAt:   now,
		TokenLimit:  tokenLimit,
		CreditLimit: creditLimit,
	}
	_, err := db.Exec(
		`INSERT INTO user_api_keys(id, user_id, name, key, enabled, created_at, token_limit, credit_limit)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.UserID, nullableString(k.Name), k.Key, boolToInt(k.Enabled), k.CreatedAt, k.TokenLimit, k.CreditLimit,
	)
	if err != nil {
		if isUniqueErr(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	return k, nil
}

// ListApiKeysForUser returns every key owned by userID, newest first.
func ListApiKeysForUser(userID string) ([]UserApiKey, error) {
	rows, err := db.Query(
		`SELECT id, user_id, IFNULL(name,''), key, enabled, created_at, IFNULL(last_used_at,0),
		        token_limit, credit_limit, tokens_used, credits_used, requests_count
		 FROM user_api_keys WHERE user_id = ? ORDER BY created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserApiKey
	for rows.Next() {
		k, err := scanApiKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// ListAllApiKeys returns all user-issued keys (admin view).
func ListAllApiKeys() ([]UserApiKey, error) {
	if db == nil {
		return nil, nil
	}
	rows, err := db.Query(
		`SELECT id, user_id, IFNULL(name,''), key, enabled, created_at, IFNULL(last_used_at,0),
		        token_limit, credit_limit, tokens_used, credits_used, requests_count
		 FROM user_api_keys ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserApiKey
	for rows.Next() {
		k, err := scanApiKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// FindApiKeyByValue returns the key whose value exactly matches.
func FindApiKeyByValue(key string) (*UserApiKey, error) {
	if key == "" || db == nil {
		return nil, ErrNotFound
	}
	row := db.QueryRow(
		`SELECT id, user_id, IFNULL(name,''), key, enabled, created_at, IFNULL(last_used_at,0),
		        token_limit, credit_limit, tokens_used, credits_used, requests_count
		 FROM user_api_keys WHERE key = ?`,
		key,
	)
	return scanApiKey(row)
}

// GetApiKeyByID returns the key by primary id.
func GetApiKeyByID(id string) (*UserApiKey, error) {
	if db == nil {
		return nil, ErrNotFound
	}
	row := db.QueryRow(
		`SELECT id, user_id, IFNULL(name,''), key, enabled, created_at, IFNULL(last_used_at,0),
		        token_limit, credit_limit, tokens_used, credits_used, requests_count
		 FROM user_api_keys WHERE id = ?`,
		id,
	)
	return scanApiKey(row)
}

// UpdateApiKey patches mutable fields. Set name/limits as desired; nil pointers
// mean "leave alone".
func UpdateApiKey(id string, name *string, enabled *bool, tokenLimit *int64, creditLimit *float64) error {
	sets := []string{}
	args := []interface{}{}
	if name != nil {
		sets = append(sets, "name = ?")
		args = append(args, nullableString(*name))
	}
	if enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, boolToInt(*enabled))
	}
	if tokenLimit != nil {
		sets = append(sets, "token_limit = ?")
		args = append(args, *tokenLimit)
	}
	if creditLimit != nil {
		sets = append(sets, "credit_limit = ?")
		args = append(args, *creditLimit)
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, id)
	q := "UPDATE user_api_keys SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	res, err := db.Exec(q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteApiKey removes the key.
func DeleteApiKey(id string) error {
	_, err := db.Exec(`DELETE FROM user_api_keys WHERE id = ?`, id)
	return err
}

// RecordApiKeyUsage atomically increments counters after a successful proxied request.
func RecordApiKeyUsage(id string, tokens int64, credits float64) error {
	_, err := db.Exec(
		`UPDATE user_api_keys
		 SET tokens_used = tokens_used + ?,
		     credits_used = credits_used + ?,
		     requests_count = requests_count + 1,
		     last_used_at = ?
		 WHERE id = ?`,
		maxInt64(tokens, 0), maxFloat(credits, 0), time.Now().Unix(), id,
	)
	return err
}

// ResetApiKeyUsage clears counters but keeps the key.
func ResetApiKeyUsage(id string) error {
	_, err := db.Exec(
		`UPDATE user_api_keys SET tokens_used = 0, credits_used = 0, requests_count = 0 WHERE id = ?`,
		id,
	)
	return err
}

// ApiKeyOverLimit reports whether the key has exhausted any non-zero limit.
func ApiKeyOverLimit(k UserApiKey) (overToken, overCredit bool) {
	if k.TokenLimit > 0 && k.TokensUsed >= k.TokenLimit {
		overToken = true
	}
	if k.CreditLimit > 0 && k.CreditsUsed >= k.CreditLimit {
		overCredit = true
	}
	return
}

// ====================== Sessions ======================

// CreateSession issues a new opaque token bound to userID.
func CreateSession(userID string) (*Session, error) {
	tok := newToken(32)
	now := time.Now()
	s := &Session{
		Token:     tok,
		UserID:    userID,
		CreatedAt: now.Unix(),
		ExpiresAt: now.Add(sessionTTL).Unix(),
	}
	_, err := db.Exec(
		`INSERT INTO sessions(token, user_id, created_at, expires_at) VALUES(?, ?, ?, ?)`,
		s.Token, s.UserID, s.CreatedAt, s.ExpiresAt,
	)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// LookupSession returns the session if it exists and is unexpired, plus the user.
func LookupSession(token string) (*Session, *User, error) {
	if token == "" || db == nil {
		return nil, nil, ErrNotFound
	}
	var s Session
	err := db.QueryRow(
		`SELECT token, user_id, created_at, expires_at FROM sessions WHERE token = ?`, token,
	).Scan(&s.Token, &s.UserID, &s.CreatedAt, &s.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	if time.Now().Unix() >= s.ExpiresAt {
		_ = DeleteSession(token)
		return nil, nil, ErrNotFound
	}
	u, err := GetUserByID(s.UserID)
	if err != nil {
		return nil, nil, err
	}
	if !u.Enabled {
		return nil, nil, errors.New("account disabled")
	}
	return &s, u, nil
}

// DeleteSession removes a session token.
func DeleteSession(token string) error {
	_, err := db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// PurgeExpiredSessions removes all rows whose expires_at is in the past.
func PurgeExpiredSessions() error {
	_, err := db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
	return err
}

// ====================== Helpers ======================

// GenerateApiKeyValue returns a fresh sk-<hex> token.
func GenerateApiKeyValue() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return "sk-" + hex.EncodeToString(buf)
}

// MaskApiKey produces a display-friendly masked version.
func MaskApiKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 10 {
		return key
	}
	return key[:6] + "****" + key[len(key)-4:]
}

// ConstantTimeEqual is a small helper for callers that want to compare opaque
// tokens defensively.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func newID() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

func newToken(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanUser(s rowScanner) (*User, error) {
	var (
		u       User
		enabled int
	)
	err := s.Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.Role, &enabled, &u.CreatedAt, &u.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.Enabled = enabled != 0
	return &u, nil
}

func scanApiKey(s rowScanner) (*UserApiKey, error) {
	var (
		k       UserApiKey
		enabled int
	)
	err := s.Scan(&k.ID, &k.UserID, &k.Name, &k.Key, &enabled, &k.CreatedAt, &k.LastUsedAt,
		&k.TokenLimit, &k.CreditLimit, &k.TokensUsed, &k.CreditsUsed, &k.RequestsCount)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	k.Enabled = enabled != 0
	return &k, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func isUniqueErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE") ||
		strings.Contains(msg, "constraint failed (UNIQUE")
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
