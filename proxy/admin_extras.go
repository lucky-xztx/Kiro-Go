package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/providers"
	"kiro-go/store"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ----- Request logs -----

func (h *Handler) apiAdminListLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	logs, err := store.ListRequestLogs(store.LogQuery{
		ApiKeyID:  q.Get("apiKeyId"),
		UserID:    q.Get("userId"),
		AccountID: q.Get("accountId"),
		Since:     since,
		Limit:     limit,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"logs": logs})
}

// ----- Usage timeseries -----

func (h *Handler) apiAdminUsageSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	days, _ := strconv.Atoi(q.Get("days"))
	if days <= 0 {
		days = 7
	}
	if days > 90 {
		days = 90
	}
	points, err := store.UsageSeries(q.Get("apiKeyId"), days)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"points": points, "days": days})
}

// ----- Model aliases -----

func (h *Handler) apiAdminListAliases(w http.ResponseWriter, r *http.Request) {
	list, err := store.ListModelAliases()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"aliases": list})
}

func (h *Handler) apiAdminUpsertAlias(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Alias    string `json:"alias"`
		Target   string `json:"target"`
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "请求体不合法")
		return
	}
	body.Alias = strings.TrimSpace(body.Alias)
	body.Target = strings.TrimSpace(body.Target)
	if body.Alias == "" || body.Target == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_input", "alias 和 target 都是必填的")
		return
	}
	if err := store.UpsertModelAlias(body.Alias, body.Target, body.Provider); err != nil {
		writeJSONError(w, http.StatusBadRequest, "save_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func (h *Handler) apiAdminDeleteAlias(w http.ResponseWriter, r *http.Request, alias string) {
	if err := store.DeleteModelAlias(alias); err != nil {
		writeJSONError(w, http.StatusBadRequest, "delete_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// ----- Providers (read-only) -----

func (h *Handler) apiAdminListProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"providers": providers.All()})
}

// ----- Account health -----

func (h *Handler) apiAdminListHealth(w http.ResponseWriter, r *http.Request) {
	rows, err := store.ListAccountHealth()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"health": rows})
}

// apiAdminRunHealthCheck triggers an on-demand sweep over the configured Kiro
// accounts. It uses the existing ensureValidToken path as the probe — if the
// account's refresh token still works, we mark it healthy; otherwise we bump
// fail_streak and (after 3 in a row) auto-disable.
func (h *Handler) apiAdminRunHealthCheck(w http.ResponseWriter, r *http.Request) {
	go h.runHealthCheckOnce("manual")
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "started": true})
}

const healthAutoDisableStreak = 3

// runHealthCheckOnce probes every account once and persists the result.
// reason is recorded in the log line so we can tell manual vs scheduled runs.
func (h *Handler) runHealthCheckOnce(reason string) {
	logger.Debugf("[Health] starting check (reason=%s)", reason)
	accounts := config.GetAccounts()
	for i := range accounts {
		a := accounts[i]
		if !a.Enabled {
			continue
		}
		// Only kiro accounts are wired today; mark others as "ok" so the page
		// doesn't show alarms for stub accounts.
		if providers.Normalize(a.Upstream) != "kiro" {
			_ = store.UpsertAccountHealth(store.AccountHealth{
				AccountID:   a.ID,
				Status:      store.HealthStatusOK,
				LastCheckAt: time.Now().Unix(),
				LastOkAt:    time.Now().Unix(),
				FailStreak:  0,
			})
			continue
		}
		err := h.ensureValidToken(&a)
		if err == nil {
			_ = store.UpsertAccountHealth(store.AccountHealth{
				AccountID:   a.ID,
				Status:      store.HealthStatusOK,
				LastCheckAt: time.Now().Unix(),
				LastOkAt:    time.Now().Unix(),
				FailStreak:  0,
			})
			continue
		}
		prev, _ := store.GetAccountHealth(a.ID)
		streak := 1
		var lastOk int64
		var autoDisabled bool
		if prev != nil {
			streak = prev.FailStreak + 1
			lastOk = prev.LastOkAt
			autoDisabled = prev.AutoDisabled
		}
		status := store.HealthStatusDegraded
		if streak >= healthAutoDisableStreak {
			status = store.HealthStatusFailing
			if a.Enabled && !autoDisabled {
				if updErr := config.SetAccountEnabled(a.ID, false); updErr != nil {
					logger.Warnf("[Health] auto-disable %s failed: %v", a.ID, updErr)
				} else {
					autoDisabled = true
					logger.Warnf("[Health] auto-disabled account %s after %d failures: %v", a.ID, streak, err)
				}
			}
		}
		_ = store.UpsertAccountHealth(store.AccountHealth{
			AccountID:    a.ID,
			Status:       status,
			LastCheckAt:  time.Now().Unix(),
			LastOkAt:     lastOk,
			FailStreak:   streak,
			LastError:    err.Error(),
			AutoDisabled: autoDisabled,
		})
	}
	logger.Debugf("[Health] check finished (reason=%s)", reason)
}

// StartHealthCheckLoop runs runHealthCheckOnce every interval. Cancellation
// happens implicitly when the process exits.
func (h *Handler) StartHealthCheckLoop(interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		// Skip the first tick — give the process time to settle.
		time.Sleep(interval)
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			h.runHealthCheckOnce("scheduled")
		}
	}()
}
