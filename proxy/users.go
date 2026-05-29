package proxy

import (
	"encoding/json"
	"errors"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/store"
	"net/http"
	"strings"
	"time"
)

// SessionCookieName is the cookie that carries the opaque session token.
const SessionCookieName = "kiro_session"

// userContextKey holds the *store.User on the request context.
type userContextKey struct{}

// requireAuth resolves the current user. Returns the user (or nil) and writes a
// 401 response when authRequired is true and resolution fails.
//
// Resolution order:
//  1. Cookie `kiro_session` → SQLite session row
//  2. Legacy admin password (header X-Admin-Password or cookie admin_password)
//     against config.Password — bound to the SQLite admin user, kept so older
//     clients keep working without an extra round-trip.
func (h *Handler) resolveUser(w http.ResponseWriter, r *http.Request, authRequired bool) *store.User {
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		if _, u, err := store.LookupSession(c.Value); err == nil {
			return u
		}
	}
	pw := r.Header.Get("X-Admin-Password")
	if pw == "" {
		if c, err := r.Cookie("admin_password"); err == nil {
			pw = c.Value
		}
	}
	if pw != "" && pw == config.GetPassword() {
		if u, err := store.GetUserByUsername("admin"); err == nil {
			return u
		}
	}
	if authRequired {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized", "请先登录")
	}
	return nil
}

// requireAdmin returns the user only if they are an admin; otherwise writes 403.
func (h *Handler) requireAdmin(w http.ResponseWriter, r *http.Request) *store.User {
	u := h.resolveUser(w, r, true)
	if u == nil {
		return nil
	}
	if u.Role != "admin" {
		writeJSONError(w, http.StatusForbidden, "forbidden", "需要管理员权限")
		return nil
	}
	return u
}

// adminAuthorized reports whether the request carries valid admin credentials.
// Accepts: (1) session cookie tied to a user with role=admin, or (2) the legacy
// X-Admin-Password header / admin_password cookie matching config.Password.
func (h *Handler) adminAuthorized(r *http.Request) bool {
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		if _, u, err := store.LookupSession(c.Value); err == nil && u != nil && u.Role == "admin" && u.Enabled {
			return true
		}
	}
	pw := r.Header.Get("X-Admin-Password")
	if pw == "" {
		if c, err := r.Cookie("admin_password"); err == nil {
			pw = c.Value
		}
	}
	if pw != "" && pw == config.GetPassword() {
		return true
	}
	return false
}

// handleUserAPI dispatches /api/* (NOT /admin/api/*) routes related to users
// and per-user API keys. Returns true when it has handled the request so the
// main router can fall through.
func (h *Handler) handleUserAPI(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/api/") {
		return false
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	switch {
	case path == "/api/register" && r.Method == "POST":
		h.apiRegister(w, r)
	case path == "/api/login" && r.Method == "POST":
		h.apiLogin(w, r)
	case path == "/api/logout" && r.Method == "POST":
		h.apiLogout(w, r)
	case path == "/api/me" && r.Method == "GET":
		h.apiMe(w, r)
	case path == "/api/me/password" && r.Method == "POST":
		h.apiMeChangePassword(w, r)
	case path == "/api/me/keys" && r.Method == "GET":
		h.apiListMyKeys(w, r)
	case path == "/api/me/keys" && r.Method == "POST":
		h.apiCreateMyKey(w, r)
	case strings.HasPrefix(path, "/api/me/keys/") && r.Method == "DELETE":
		id := strings.TrimPrefix(path, "/api/me/keys/")
		h.apiDeleteMyKey(w, r, id)
	case strings.HasPrefix(path, "/api/me/keys/") && r.Method == "PUT":
		id := strings.TrimPrefix(path, "/api/me/keys/")
		h.apiUpdateMyKey(w, r, id)
	case path == "/api/me/logs" && r.Method == "GET":
		h.apiListMyLogs(w, r)
	default:
		// Public endpoints (already handled in the main router) shouldn't reach
		// here; for everything else under /api/, signal "not handled".
		return false
	}
	return true
}

// ---------------------- Auth endpoints ----------------------

func (h *Handler) apiRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "请求体不合法")
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if body.Username == "" || body.Password == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_input", "用户名和密码不能为空")
		return
	}
	if len(body.Username) < 3 || len(body.Username) > 32 {
		writeJSONError(w, http.StatusBadRequest, "invalid_input", "用户名长度需在 3-32 之间")
		return
	}
	u, err := store.CreateUser(body.Username, body.Email, body.Password, "user")
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeJSONError(w, http.StatusConflict, "conflict", "用户名已存在")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "create_failed", err.Error())
		return
	}
	if err := writeSession(w, r, u); err != nil {
		logger.Warnf("write session: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"user": publicUser(u)})
}

func (h *Handler) apiLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "请求体不合法")
		return
	}
	body.Username = strings.TrimSpace(body.Username)

	// Backwards-compat: allow logging in with the legacy admin password as the
	// `admin` user even if no password was set on the SQLite admin row yet.
	if strings.EqualFold(body.Username, "admin") && body.Password == config.GetPassword() && body.Password != "" {
		u, err := store.GetUserByUsername("admin")
		if err == nil {
			_ = writeSession(w, r, u)
			writeJSON(w, http.StatusOK, map[string]interface{}{"user": publicUser(u)})
			return
		}
	}

	u, err := store.VerifyPassword(body.Username, body.Password)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid_credentials", "用户名或密码错误")
		return
	}
	if err := writeSession(w, r, u); err != nil {
		logger.Warnf("write session: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"user": publicUser(u)})
}

func (h *Handler) apiLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		_ = store.DeleteSession(c.Value)
	}
	clearSessionCookie(w, r)
	// Also clear legacy admin_password cookie so refresh actually logs out.
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_password",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func (h *Handler) apiMe(w http.ResponseWriter, r *http.Request) {
	u := h.resolveUser(w, r, false)
	if u == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"user": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"user": publicUser(u)})
}

func (h *Handler) apiMeChangePassword(w http.ResponseWriter, r *http.Request) {
	u := h.resolveUser(w, r, true)
	if u == nil {
		return
	}
	var body struct {
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "请求体不合法")
		return
	}
	if _, err := store.VerifyPassword(u.Username, body.OldPassword); err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid_credentials", "原密码不正确")
		return
	}
	if err := store.UpdateUserPassword(u.ID, body.NewPassword); err != nil {
		writeJSONError(w, http.StatusBadRequest, "update_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// ---------------------- Per-user API keys ----------------------

func (h *Handler) apiListMyKeys(w http.ResponseWriter, r *http.Request) {
	u := h.resolveUser(w, r, true)
	if u == nil {
		return
	}
	keys, err := store.ListApiKeysForUser(u.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"keys": publicKeys(keys, false)})
}

func (h *Handler) apiCreateMyKey(w http.ResponseWriter, r *http.Request) {
	u := h.resolveUser(w, r, true)
	if u == nil {
		return
	}
	var body struct {
		Name        string  `json:"name"`
		TokenLimit  int64   `json:"tokenLimit"`
		CreditLimit float64 `json:"creditLimit"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	k, err := store.CreateApiKey(u.ID, body.Name, "", body.TokenLimit, body.CreditLimit)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "create_failed", err.Error())
		return
	}
	// Return the FULL key once, immediately after creation.
	writeJSON(w, http.StatusOK, map[string]interface{}{"key": publicKey(*k, true)})
}

func (h *Handler) apiUpdateMyKey(w http.ResponseWriter, r *http.Request, id string) {
	u := h.resolveUser(w, r, true)
	if u == nil {
		return
	}
	existing, err := store.GetApiKeyByID(id)
	if err != nil || existing.UserID != u.ID {
		writeJSONError(w, http.StatusNotFound, "not_found", "Key 不存在")
		return
	}
	var body struct {
		Name        *string  `json:"name"`
		Enabled     *bool    `json:"enabled"`
		TokenLimit  *int64   `json:"tokenLimit"`
		CreditLimit *float64 `json:"creditLimit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "请求体不合法")
		return
	}
	if err := store.UpdateApiKey(id, body.Name, body.Enabled, body.TokenLimit, body.CreditLimit); err != nil {
		writeJSONError(w, http.StatusBadRequest, "update_failed", err.Error())
		return
	}
	updated, _ := store.GetApiKeyByID(id)
	writeJSON(w, http.StatusOK, map[string]interface{}{"key": publicKey(*updated, false)})
}

func (h *Handler) apiDeleteMyKey(w http.ResponseWriter, r *http.Request, id string) {
	u := h.resolveUser(w, r, true)
	if u == nil {
		return
	}
	existing, err := store.GetApiKeyByID(id)
	if err != nil || existing.UserID != u.ID {
		writeJSONError(w, http.StatusNotFound, "not_found", "Key 不存在")
		return
	}
	if err := store.DeleteApiKey(id); err != nil {
		writeJSONError(w, http.StatusBadRequest, "delete_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// ---------------------- Admin: user management ----------------------

func (h *Handler) apiAdminListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := store.ListUsers()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]map[string]interface{}, 0, len(users))
	for _, u := range users {
		uu := u
		row := publicUser(&uu)
		// Attach key count for the table.
		if keys, _ := store.ListApiKeysForUser(u.ID); keys != nil {
			row["keysCount"] = len(keys)
		} else {
			row["keysCount"] = 0
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"users": out})
}

func (h *Handler) apiAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "请求体不合法")
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if body.Username == "" || body.Password == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_input", "用户名和密码不能为空")
		return
	}
	role := body.Role
	if role != "admin" {
		role = "user"
	}
	u, err := store.CreateUser(body.Username, body.Email, body.Password, role)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeJSONError(w, http.StatusConflict, "conflict", "用户名已存在")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "create_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"user": publicUser(u)})
}

func (h *Handler) apiAdminResetUserPassword(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		NewPassword string `json:"newPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "请求体不合法")
		return
	}
	if err := store.UpdateUserPassword(id, body.NewPassword); err != nil {
		writeJSONError(w, http.StatusBadRequest, "update_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func (h *Handler) apiAdminSetUserRole(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "请求体不合法")
		return
	}
	role := body.Role
	if role != "admin" && role != "user" {
		writeJSONError(w, http.StatusBadRequest, "invalid_role", "角色必须为 admin 或 user")
		return
	}
	target, err := store.GetUserByID(id)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "not_found", "用户不存在")
		return
	}
	// Don't allow demoting the last admin.
	if target.Role == "admin" && role == "user" {
		users, _ := store.ListUsers()
		admins := 0
		for _, u := range users {
			if u.Role == "admin" {
				admins++
			}
		}
		if admins <= 1 {
			writeJSONError(w, http.StatusBadRequest, "last_admin", "不能降级最后一个管理员")
			return
		}
	}
	if err := store.SetUserRole(id, role); err != nil {
		writeJSONError(w, http.StatusBadRequest, "update_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func (h *Handler) apiAdminSetUserEnabled(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "请求体不合法")
		return
	}
	target, err := store.GetUserByID(id)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "not_found", "用户不存在")
		return
	}
	if target.Role == "admin" && !body.Enabled {
		users, _ := store.ListUsers()
		active := 0
		for _, u := range users {
			if u.Role == "admin" && u.Enabled {
				active++
			}
		}
		if active <= 1 {
			writeJSONError(w, http.StatusBadRequest, "last_admin", "不能停用最后一个管理员")
			return
		}
	}
	if err := store.SetUserEnabled(id, body.Enabled); err != nil {
		writeJSONError(w, http.StatusBadRequest, "update_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func (h *Handler) apiAdminDeleteUser(w http.ResponseWriter, r *http.Request, id string) {
	target, err := store.GetUserByID(id)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "not_found", "用户不存在")
		return
	}
	if target.Role == "admin" {
		users, _ := store.ListUsers()
		admins := 0
		for _, u := range users {
			if u.Role == "admin" {
				admins++
			}
		}
		if admins <= 1 {
			writeJSONError(w, http.StatusBadRequest, "last_admin", "不能删除最后一个管理员")
			return
		}
	}
	if err := store.DeleteUser(id); err != nil {
		writeJSONError(w, http.StatusBadRequest, "delete_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func (h *Handler) apiAdminListUserKeys(w http.ResponseWriter, r *http.Request, id string) {
	keys, err := store.ListApiKeysForUser(id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"keys": publicKeys(keys, false)})
}

// ---------------------- Helpers ----------------------

func writeSession(w http.ResponseWriter, r *http.Request, u *store.User) error {
	s, err := store.CreateSession(u.ID)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    s.Token,
		Path:     "/",
		Expires:  time.Unix(s.ExpiresAt, 0),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
	})
	return nil
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
	})
}

func isHTTPS(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]string{"code": code, "message": message},
	})
}

func publicUser(u *store.User) map[string]interface{} {
	if u == nil {
		return nil
	}
	return map[string]interface{}{
		"id":        u.ID,
		"username":  u.Username,
		"email":     u.Email,
		"role":      u.Role,
		"enabled":   u.Enabled,
		"createdAt": u.CreatedAt,
	}
}

func publicKey(k store.UserApiKey, includeSecret bool) map[string]interface{} {
	out := map[string]interface{}{
		"id":            k.ID,
		"name":          k.Name,
		"masked":        store.MaskApiKey(k.Key),
		"enabled":       k.Enabled,
		"createdAt":     k.CreatedAt,
		"lastUsedAt":    k.LastUsedAt,
		"tokenLimit":    k.TokenLimit,
		"creditLimit":   k.CreditLimit,
		"tokensUsed":    k.TokensUsed,
		"creditsUsed":   k.CreditsUsed,
		"requestsCount": k.RequestsCount,
	}
	if includeSecret {
		out["key"] = k.Key
	}
	return out
}

func publicKeys(keys []store.UserApiKey, includeSecret bool) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(keys))
	for _, k := range keys {
		out = append(out, publicKey(k, includeSecret))
	}
	return out
}
