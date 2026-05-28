package proxy

import (
	"context"
	"kiro-go/config"
	"kiro-go/store"
	"net/http"
	"strings"
)

// apiKeyContextKey is an unexported type used as the context key for the matched ApiKeyEntry
// so it cannot collide with keys defined in other packages.
type apiKeyContextKey struct{}

// userApiKeyContextKey carries the matched per-user store.UserApiKey ID.
type userApiKeyContextKey struct{}

// authError describes why authentication failed. status is the HTTP status code to send.
type authError struct {
	status  int
	code    string
	message string
}

func (e *authError) Error() string { return e.message }

func newAuthError(status int, code, message string) *authError {
	return &authError{status: status, code: code, message: message}
}

// extractProvidedKey reads the API key from Authorization (Bearer ...) or X-Api-Key header.
func extractProvidedKey(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		return strings.TrimPrefix(authHeader, "Bearer ")
	}
	if v := r.Header.Get("X-Api-Key"); v != "" {
		return v
	}
	return ""
}

// authenticate validates an incoming request against the configured API keys.
//
// Auth is now ALWAYS required. Resolution order:
//  1. SQLite per-user key store (where every Key issued via the UI lives).
//  2. Legacy config.ApiKeys list (only present briefly before migration runs).
//  3. Legacy single config.ApiKey field (oldest deployments).
//
// Returns (legacyEntry, contextualizedRequest, nil) on success. legacyEntry is
// non-nil only when the key matched a legacy config entry; for user keys, the
// returned request carries the matched key ID in its context.
func (h *Handler) authenticate(r *http.Request) (*config.ApiKeyEntry, *http.Request, error) {
	provided := extractProvidedKey(r)
	if provided == "" {
		return nil, r, newAuthError(http.StatusUnauthorized, "authentication_error", "Invalid or missing API key")
	}

	// Try the per-user store first.
	if k, err := store.FindApiKeyByValue(provided); err == nil {
		if !k.Enabled {
			return nil, r, newAuthError(http.StatusUnauthorized, "authentication_error", "API key disabled")
		}
		if overToken, overCredit := store.ApiKeyOverLimit(*k); overToken || overCredit {
			if overToken {
				return nil, r, newAuthError(http.StatusTooManyRequests, "rate_limit_error", "token limit exceeded")
			}
			return nil, r, newAuthError(http.StatusTooManyRequests, "rate_limit_error", "credit limit exceeded")
		}
		if owner, err := store.GetUserByID(k.UserID); err == nil && !owner.Enabled {
			return nil, r, newAuthError(http.StatusUnauthorized, "authentication_error", "Account disabled")
		}
		ctx := context.WithValue(r.Context(), userApiKeyContextKey{}, k.ID)
		return nil, r.WithContext(ctx), nil
	}

	// Legacy multi-key list.
	if config.HasApiKeys() {
		entry := config.FindApiKeyByValue(provided)
		if entry == nil {
			return nil, r, newAuthError(http.StatusUnauthorized, "authentication_error", "Invalid or missing API key")
		}
		if !entry.Enabled {
			return nil, r, newAuthError(http.StatusUnauthorized, "authentication_error", "API key disabled")
		}
		if overToken, overCredit := config.ApiKeyOverLimit(*entry); overToken || overCredit {
			if overToken {
				return nil, r, newAuthError(http.StatusTooManyRequests, "rate_limit_error", "token limit exceeded")
			}
			return nil, r, newAuthError(http.StatusTooManyRequests, "rate_limit_error", "credit limit exceeded")
		}
		return entry, r, nil
	}

	// Legacy single-key path.
	expected := config.GetApiKey()
	if expected == "" || provided != expected {
		return nil, r, newAuthError(http.StatusUnauthorized, "authentication_error", "Invalid or missing API key")
	}
	return nil, r, nil
}

// hasAnyUserApiKeys reports whether at least one user-issued API key exists.
// Kept for diagnostics; auth no longer depends on this.
func (h *Handler) hasAnyUserApiKeys() bool {
	keys, err := store.ListAllApiKeys()
	if err != nil {
		return true
	}
	return len(keys) > 0
}

// withApiKeyContext attaches the matched legacy entry to the request context so
// downstream handlers (recordSuccess, etc.) can credit usage against the correct key.
func withApiKeyContext(r *http.Request, entry *config.ApiKeyEntry) *http.Request {
	if entry == nil {
		return r
	}
	ctx := context.WithValue(r.Context(), apiKeyContextKey{}, entry.ID)
	return r.WithContext(ctx)
}

// apiKeyIDFromContext returns the matched legacy API key ID stored in ctx, or empty string.
func apiKeyIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(apiKeyContextKey{}).(string); ok {
		return v
	}
	return ""
}

// userApiKeyIDFromContext returns the matched per-user (SQLite) API key ID, if any.
func userApiKeyIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(userApiKeyContextKey{}).(string); ok {
		return v
	}
	return ""
}

// activeApiKeyID returns whichever ID we should attribute usage to.
// Per-user keys take precedence over the legacy config keys.
func activeApiKeyID(ctx context.Context) string {
	if id := userApiKeyIDFromContext(ctx); id != "" {
		return id
	}
	return apiKeyIDFromContext(ctx)
}
