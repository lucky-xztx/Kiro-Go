// Package store 提供 SQLite 持久化存储，管理用户账号、
// 用户 API Key 和登录会话。
//
// Kiro 上游账号池和全局服务设置仍在 data/config.json 中，
// 本包只负责面向用户的身份数据。
//
// 驱动：modernc.org/sqlite（纯 Go，无需 CGO）。
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

// User 表示一个注册用户。
type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	Email        string `json:"email,omitempty"`
	PasswordHash string `json:"-"`
	Role         string `json:"role"` // "admin" 或 "user"
	Enabled      bool   `json:"enabled"`
	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
}

// UserApiKey 表示颁发给用户的单个 API Key。
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

// Session 是绑定到 cookie 的服务端会话记录。
type Session struct {
	Token     string
	UserID    string
	CreatedAt int64
	ExpiresAt int64
}

var (
	db   *sql.DB
	once sync.Once

	// ErrNotFound 在查询不到记录时返回。
	ErrNotFound = errors.New("not found")
	// ErrConflict 在唯一约束冲突时返回，调用方可转换为"已存在"提示。
	ErrConflict = errors.New("conflict")
)

const (
	sessionTTL = 30 * 24 * time.Hour // 30 天
)

// Init 打开（或创建）指定路径的 SQLite 数据库并应用表结构。
// 可安全多次调用，只有首次调用会执行初始化。
func Init(path string) error {
	var initErr error
	once.Do(func() {
		// _pragma 选项在连接时启用 WAL 模式、外键约束
		dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", path)
		conn, err := sql.Open("sqlite", dsn)
		if err != nil {
			initErr = fmt.Errorf("open sqlite: %w", err)
			return
		}
		// SQLite 在文件级别处理并发，小连接池即可
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

// Close 释放 SQLite 连接池。
// 在优雅关闭时调用，确保 WAL 文件被正确刷盘。
func Close() error {
	if db != nil {
		return db.Close()
	}
	return nil
}

// DB 返回底层的 *sql.DB，主要用于测试。
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

// CreateUser 插入新用户，密码使用 bcrypt 哈希。
// 用户名重复时返回 ErrConflict。
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

// GetUserByUsername 按用户名查找（不区分大小写），未找到返回 ErrNotFound。
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

// GetUserByID 按 ID 查找用户，未找到返回 ErrNotFound。
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

// ListUsers 返回所有用户，按创建时间倒序。
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

// UserCount 返回注册用户总数。
func UserCount() (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// UpdateUserPassword 替换指定用户的 bcrypt 密码哈希。
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

// SetUserEnabled 切换用户账号启用状态。
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

// SetUserRole 修改用户角色（admin/user）。
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

// DeleteUser 删除用户，其 API Key 和会话通过外键级联删除。
func DeleteUser(id string) error {
	_, err := db.Exec(`DELETE FROM users WHERE id = ?`, id)
	return err
}

// VerifyPassword 验证用户名和密码，匹配且账号启用时返回用户。
// 使用常量时间 bcrypt 比较，防止时序攻击。
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

// CreateApiKey 为指定用户颁发新 API Key。
// 如果 keyValue 为空，自动生成 sk-<hex> 格式的令牌。
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

// ListApiKeysForUser 返回指定用户的所有 Key，按创建时间倒序。
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

// ListAllApiKeys 返回所有用户颁发的 Key（管理员视图）。
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

// FindApiKeyByValue 按精确值查找 API Key。
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

// GetApiKeyByID 按主键 ID 查找 API Key。
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

// UpdateApiKey 局部更新 API Key 的可变字段。
// nil 指针表示"不修改该字段"。
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

// DeleteApiKey 删除指定的 API Key。
func DeleteApiKey(id string) error {
	_, err := db.Exec(`DELETE FROM user_api_keys WHERE id = ?`, id)
	return err
}

// RecordApiKeyUsage 在代理请求成功后原子递增计数器。
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

// ResetApiKeyUsage 清零计数器，保留 Key。
func ResetApiKeyUsage(id string) error {
	_, err := db.Exec(
		`UPDATE user_api_keys SET tokens_used = 0, credits_used = 0, requests_count = 0 WHERE id = ?`,
		id,
	)
	return err
}

// ApiKeyOverLimit 检查 Key 是否已超出任一非零额度限制。
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

// CreateSession 为指定用户创建新的会话令牌。
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

// LookupSession 查找会话，若存在且未过期则返回会话和关联用户。
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

// DeleteSession 删除指定的会话令牌。
func DeleteSession(token string) error {
	_, err := db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// PurgeExpiredSessions 清除所有已过期的会话。
func PurgeExpiredSessions() error {
	_, err := db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
	return err
}


// GenerateApiKeyValue 生成新的 sk-<hex> 格式令牌。
func GenerateApiKeyValue() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return "sk-" + hex.EncodeToString(buf)
}

// MaskApiKey 生成用于展示的脱敏版本。
func MaskApiKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 10 {
		return key
	}
	return key[:6] + "****" + key[len(key)-4:]
}

// ConstantTimeEqual 用于安全比较不透明令牌，防止时序攻击。
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
