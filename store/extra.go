package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ====================== Request logs ======================

// RequestLog 表示一条请求日志记录。
type RequestLog struct {
	ID           string  `json:"id"`
	CreatedAt    int64   `json:"createdAt"`
	ApiKeyID     string  `json:"apiKeyId,omitempty"`
	UserID       string  `json:"userId,omitempty"`
	AccountID    string  `json:"accountId,omitempty"`
	Provider     string  `json:"provider,omitempty"`
	Model        string  `json:"model,omitempty"`
	Path         string  `json:"path,omitempty"`
	Status       int     `json:"status"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	Credits      float64 `json:"credits"`
	LatencyMs    int64   `json:"latencyMs"`
	Error        string  `json:"error,omitempty"`
}

// LogRequest 持久化一条请求日志。错误会返回但调用方通常只记录并忽略——请求处理不能依赖此操作。
func LogRequest(l RequestLog) error {
	if db == nil {
		return nil
	}
	if l.ID == "" {
		l.ID = newID()
	}
	if l.CreatedAt == 0 {
		l.CreatedAt = time.Now().Unix()
	}
	_, err := db.Exec(
		`INSERT INTO request_logs(id, created_at, api_key_id, user_id, account_id,
			provider, model, path, status, input_tokens, output_tokens, credits,
			latency_ms, error)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.CreatedAt,
		nullableString(l.ApiKeyID),
		nullableString(l.UserID),
		nullableString(l.AccountID),
		nullableString(l.Provider),
		nullableString(l.Model),
		nullableString(l.Path),
		l.Status,
		l.InputTokens, l.OutputTokens, l.Credits, l.LatencyMs,
		nullableString(l.Error),
	)
	return err
}

// LogQuery 请求日志查询筛选条件。
type LogQuery struct {
	ApiKeyID  string
	UserID    string
	AccountID string
	Since     int64 // unix 时间戳，0 表示无下界
	Limit     int   // 默认 100，最大 1000
	Offset    int   // 分页偏移
}

// CountRequestLogs 返回匹配查询条件的总行数（忽略 Limit/Offset）。
func CountRequestLogs(q LogQuery) (int64, error) {
	if db == nil {
		return 0, nil
	}
	args := []interface{}{}
	wheres := []string{}
	if q.ApiKeyID != "" {
		wheres = append(wheres, "api_key_id = ?")
		args = append(args, q.ApiKeyID)
	}
	if q.UserID != "" {
		wheres = append(wheres, "user_id = ?")
		args = append(args, q.UserID)
	}
	if q.AccountID != "" {
		wheres = append(wheres, "account_id = ?")
		args = append(args, q.AccountID)
	}
	if q.Since > 0 {
		wheres = append(wheres, "created_at >= ?")
		args = append(args, q.Since)
	}
	where := ""
	if len(wheres) > 0 {
		where = " WHERE " + strings.Join(wheres, " AND ")
	}
	var n int64
	row := db.QueryRow(`SELECT COUNT(*) FROM request_logs`+where, args...)
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// RateStats 聚合指定时间窗口内的请求数和 Token 总量。
type RateStats struct {
	Requests int64 `json:"requests"`
	Tokens   int64 `json:"tokens"`
}

// RequestRate 返回指定时间窗口 [since, now] 内的请求数和 Token 总量。用于 RPM/TPM 指标。
func RequestRate(q LogQuery, since int64) (RateStats, error) {
	var s RateStats
	if db == nil {
		return s, nil
	}
	args := []interface{}{since}
	wheres := []string{"created_at >= ?"}
	if q.ApiKeyID != "" {
		wheres = append(wheres, "api_key_id = ?")
		args = append(args, q.ApiKeyID)
	}
	if q.UserID != "" {
		wheres = append(wheres, "user_id = ?")
		args = append(args, q.UserID)
	}
	row := db.QueryRow(
		`SELECT COUNT(*), IFNULL(SUM(input_tokens + output_tokens), 0)
		   FROM request_logs WHERE `+strings.Join(wheres, " AND "),
		args...)
	if err := row.Scan(&s.Requests, &s.Tokens); err != nil {
		return s, err
	}
	return s, nil
}

func ListRequestLogs(q LogQuery) ([]RequestLog, error) {
	if db == nil {
		return nil, nil
	}
	if q.Limit <= 0 {
		q.Limit = 100
	}
	if q.Limit > 1000 {
		q.Limit = 1000
	}
	args := []interface{}{}
	wheres := []string{}
	if q.ApiKeyID != "" {
		wheres = append(wheres, "api_key_id = ?")
		args = append(args, q.ApiKeyID)
	}
	if q.UserID != "" {
		wheres = append(wheres, "user_id = ?")
		args = append(args, q.UserID)
	}
	if q.AccountID != "" {
		wheres = append(wheres, "account_id = ?")
		args = append(args, q.AccountID)
	}
	if q.Since > 0 {
		wheres = append(wheres, "created_at >= ?")
		args = append(args, q.Since)
	}
	where := ""
	if len(wheres) > 0 {
		where = " WHERE " + strings.Join(wheres, " AND ")
	}
	args = append(args, q.Limit, q.Offset)
	rows, err := db.Query(
		`SELECT id, created_at, IFNULL(api_key_id,''), IFNULL(user_id,''), IFNULL(account_id,''),
		        IFNULL(provider,''), IFNULL(model,''), IFNULL(path,''), status,
		        input_tokens, output_tokens, credits, latency_ms, IFNULL(error,'')
		 FROM request_logs`+where+` ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RequestLog
	for rows.Next() {
		var l RequestLog
		if err := rows.Scan(&l.ID, &l.CreatedAt, &l.ApiKeyID, &l.UserID, &l.AccountID,
			&l.Provider, &l.Model, &l.Path, &l.Status,
			&l.InputTokens, &l.OutputTokens, &l.Credits, &l.LatencyMs, &l.Error); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// PruneRequestLogs 删除指定时间之前的请求日志。由调用方选择截止时间。
func PruneRequestLogs(cutoff int64) (int64, error) {
	if db == nil {
		return 0, nil
	}
	res, err := db.Exec(`DELETE FROM request_logs WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// UsagePoint 表示使用量时间序列查询中的一个数据点。
type UsagePoint struct {
	Bucket   int64   `json:"bucket"` // 时间桶起始的 unix 时间戳
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Credits  float64 `json:"credits"`
}

// UsageSeries 返回指定 API Key（空字符串表示所有 Key）在最近 days 天内的每日使用量。
func UsageSeries(apiKeyID string, days int) ([]UsagePoint, error) {
	if db == nil {
		return nil, nil
	}
	if days <= 0 {
		days = 7
	}
	if days > 90 {
		days = 90
	}
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	args := []interface{}{since}
	keyClause := ""
	if apiKeyID != "" {
		keyClause = " AND api_key_id = ?"
		args = append(args, apiKeyID)
	}
	rows, err := db.Query(
		`SELECT (created_at / 86400) * 86400 AS bucket,
		        COUNT(*) AS requests,
		        SUM(input_tokens + output_tokens) AS tokens,
		        SUM(credits) AS credits
		 FROM request_logs
		 WHERE created_at >= ?`+keyClause+`
		 GROUP BY bucket ORDER BY bucket ASC`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsagePoint
	for rows.Next() {
		var p UsagePoint
		var tokens sql.NullInt64
		var credits sql.NullFloat64
		if err := rows.Scan(&p.Bucket, &p.Requests, &tokens, &credits); err != nil {
			return nil, err
		}
		p.Tokens = tokens.Int64
		p.Credits = credits.Float64
		out = append(out, p)
	}
	return out, rows.Err()
}

// ====================== Model aliases ======================

type ModelAlias struct {
	Alias     string `json:"alias"`
	Target    string `json:"target"`
	Provider  string `json:"provider,omitempty"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

func ListModelAliases() ([]ModelAlias, error) {
	if db == nil {
		return nil, nil
	}
	rows, err := db.Query(
		`SELECT alias, target, IFNULL(provider,''), created_at, updated_at
		 FROM model_aliases ORDER BY alias ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelAlias
	for rows.Next() {
		var a ModelAlias
		if err := rows.Scan(&a.Alias, &a.Target, &a.Provider, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func UpsertModelAlias(alias, target, provider string) error {
	if db == nil {
		return errors.New("store not initialized")
	}
	alias = strings.TrimSpace(alias)
	target = strings.TrimSpace(target)
	if alias == "" || target == "" {
		return errors.New("alias and target are required")
	}
	now := time.Now().Unix()
	_, err := db.Exec(
		`INSERT INTO model_aliases(alias, target, provider, created_at, updated_at)
		 VALUES(?, ?, ?, ?, ?)
		 ON CONFLICT(alias) DO UPDATE SET target=excluded.target, provider=excluded.provider, updated_at=excluded.updated_at`,
		alias, target, nullableString(provider), now, now)
	return err
}

func DeleteModelAlias(alias string) error {
	if db == nil {
		return errors.New("store not initialized")
	}
	_, err := db.Exec(`DELETE FROM model_aliases WHERE alias = ?`, alias)
	return err
}

// ResolveModel 如果 model 是已注册的别名则返回其目标模型，否则原样返回。
func ResolveModel(model string) string {
	if db == nil || model == "" {
		return model
	}
	var target string
	err := db.QueryRow(`SELECT target FROM model_aliases WHERE alias = ?`, model).Scan(&target)
	if err != nil || target == "" {
		return model
	}
	return target
}

// ====================== Account health ======================

const (
	HealthStatusOK       = "ok"
	HealthStatusDegraded = "degraded"
	HealthStatusFailing  = "failing"
)

type AccountHealth struct {
	AccountID    string `json:"accountId"`
	Status       string `json:"status"`
	LastCheckAt  int64  `json:"lastCheckAt"`
	LastOkAt     int64  `json:"lastOkAt,omitempty"`
	FailStreak   int    `json:"failStreak"`
	LastError    string `json:"lastError,omitempty"`
	AutoDisabled bool   `json:"autoDisabled"`
}

func GetAccountHealth(accountID string) (*AccountHealth, error) {
	if db == nil || accountID == "" {
		return nil, ErrNotFound
	}
	var h AccountHealth
	var disabled int
	err := db.QueryRow(
		`SELECT account_id, status, last_check_at, IFNULL(last_ok_at,0),
		        fail_streak, IFNULL(last_error,''), auto_disabled
		 FROM account_health WHERE account_id = ?`, accountID).
		Scan(&h.AccountID, &h.Status, &h.LastCheckAt, &h.LastOkAt,
			&h.FailStreak, &h.LastError, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	h.AutoDisabled = disabled != 0
	return &h, nil
}

func ListAccountHealth() ([]AccountHealth, error) {
	if db == nil {
		return nil, nil
	}
	rows, err := db.Query(
		`SELECT account_id, status, last_check_at, IFNULL(last_ok_at,0),
		        fail_streak, IFNULL(last_error,''), auto_disabled
		 FROM account_health ORDER BY last_check_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountHealth
	for rows.Next() {
		var h AccountHealth
		var disabled int
		if err := rows.Scan(&h.AccountID, &h.Status, &h.LastCheckAt, &h.LastOkAt,
			&h.FailStreak, &h.LastError, &disabled); err != nil {
			return nil, err
		}
		h.AutoDisabled = disabled != 0
		out = append(out, h)
	}
	return out, rows.Err()
}

func UpsertAccountHealth(h AccountHealth) error {
	if db == nil {
		return errors.New("store not initialized")
	}
	if h.LastCheckAt == 0 {
		h.LastCheckAt = time.Now().Unix()
	}
	disabled := 0
	if h.AutoDisabled {
		disabled = 1
	}
	_, err := db.Exec(
		`INSERT INTO account_health(account_id, status, last_check_at, last_ok_at,
			fail_streak, last_error, auto_disabled)
		 VALUES(?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(account_id) DO UPDATE SET
			status = excluded.status,
			last_check_at = excluded.last_check_at,
			last_ok_at = COALESCE(NULLIF(excluded.last_ok_at,0), account_health.last_ok_at),
			fail_streak = excluded.fail_streak,
			last_error = excluded.last_error,
			auto_disabled = excluded.auto_disabled`,
		h.AccountID, h.Status, h.LastCheckAt, h.LastOkAt,
		h.FailStreak, nullableString(h.LastError), disabled)
	return err
}

func DeleteAccountHealth(accountID string) error {
	if db == nil {
		return nil
	}
	_, err := db.Exec(`DELETE FROM account_health WHERE account_id = ?`, accountID)
	return err
}
