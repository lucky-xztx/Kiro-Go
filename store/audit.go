package store

import (
	"strings"
	"time"
)

// ====================== Audit logs ======================

// AuditLog 表示一条操作审计记录。
type AuditLog struct {
	ID         string `json:"id"`
	CreatedAt  int64  `json:"createdAt"`
	UserID     string `json:"userId"`
	Username   string `json:"username"`
	Action     string `json:"action"`
	TargetType string `json:"targetType"`
	TargetID   string `json:"targetId,omitempty"`
	Detail     string `json:"detail,omitempty"`
	IPAddress  string `json:"ipAddress,omitempty"`
}

// AuditQuery 审计日志查询筛选条件。
type AuditQuery struct {
	UserID     string
	Action     string
	TargetType string
	Since      int64
	Limit      int
	Offset     int
}

// LogAudit 写入一条审计日志。
// 调用方通常在管理操作成功后通过 go 关键字异步调用，不阻塞主流程。
func LogAudit(l AuditLog) {
	if db == nil {
		return
	}
	if l.ID == "" {
		l.ID = newID()
	}
	if l.CreatedAt == 0 {
		l.CreatedAt = time.Now().Unix()
	}
	_, _ = db.Exec(
		`INSERT INTO audit_logs(id, created_at, user_id, username, action,
			target_type, target_id, detail, ip_address)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.CreatedAt,
		l.UserID, l.Username, l.Action,
		l.TargetType, nullableString(l.TargetID),
		nullableString(l.Detail), nullableString(l.IPAddress),
	)
}

// ListAuditLogs 按条件查询审计日志，支持分页。
func ListAuditLogs(q AuditQuery) ([]AuditLog, error) {
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
	if q.UserID != "" {
		wheres = append(wheres, "user_id = ?")
		args = append(args, q.UserID)
	}
	if q.Action != "" {
		wheres = append(wheres, "action = ?")
		args = append(args, q.Action)
	}
	if q.TargetType != "" {
		wheres = append(wheres, "target_type = ?")
		args = append(args, q.TargetType)
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
		`SELECT id, created_at, user_id, username, action,
		        target_type, IFNULL(target_id,''), IFNULL(detail,''), IFNULL(ip_address,'')
		 FROM audit_logs`+where+` ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AuditLog
	for rows.Next() {
		var l AuditLog
		if err := rows.Scan(&l.ID, &l.CreatedAt, &l.UserID, &l.Username, &l.Action,
			&l.TargetType, &l.TargetID, &l.Detail, &l.IPAddress); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// PruneAuditLogs 清除指定时间之前的审计日志。
func PruneAuditLogs(cutoff int64) (int64, error) {
	if db == nil {
		return 0, nil
	}
	res, err := db.Exec(`DELETE FROM audit_logs WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
