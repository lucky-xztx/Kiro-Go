package codex

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"kiro-go/logger"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	authURL           = "https://auth.openai.com/oauth/authorize"
	tokenURL          = "https://auth.openai.com/oauth/token"
	clientID          = "app_EMoamEEZ73f0CkXaXp7hrann"
	scopeStr          = "openid email profile offline_access"
	redirectURI       = "http://localhost:1455/auth/callback"
	deviceRedirectURI = "https://auth.openai.com/deviceauth/callback"
	deviceCodeURL     = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	deviceTokenURL    = "https://auth.openai.com/api/accounts/deviceauth/token"
	deviceVerifyURL   = "https://auth.openai.com/codex/device"
)

// TokenResponse is the JSON returned by the OpenAI OAuth token endpoint.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

// PKCECodes holds the PKCE verifier and challenge pair.
type PKCECodes struct {
	Verifier  string
	Challenge string
}

// DeviceCodeResponse is returned by the device code start endpoint.
type DeviceCodeResponse struct {
	DeviceAuthID string `json:"device_auth_id"`
	UserCode     string `json:"user_code"`
	Interval     int    `json:"interval"`
}

// ==================== PKCE ====================

// GeneratePKCE creates a new PKCE code_verifier and code_challenge pair.
func GeneratePKCE() (*PKCECodes, error) {
	verifierBytes := make([]byte, 96)
	if _, err := rand.Read(verifierBytes); err != nil {
		return nil, fmt.Errorf("generate PKCE verifier: %w", err)
	}
	verifier := base64urlEncode(verifierBytes)

	hash := sha256.Sum256([]byte(verifier))
	challenge := base64urlEncode(hash[:])

	return &PKCECodes{Verifier: verifier, Challenge: challenge}, nil
}

// ==================== Authorization URL ====================

// GenerateState generates a random state parameter for CSRF protection.
func GenerateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64urlEncode(b), nil
}

// BuildAuthorizationURL constructs the OAuth authorize URL with PKCE parameters.
func BuildAuthorizationURL(codeChallenge, state string) string {
	params := url.Values{
		"client_id":                  {clientID},
		"response_type":              {"code"},
		"redirect_uri":               {redirectURI},
		"scope":                      {scopeStr},
		"state":                      {state},
		"code_challenge":             {codeChallenge},
		"code_challenge_method":      {"S256"},
		"prompt":                     {"login"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}
	return authURL + "?" + params.Encode()
}

// ==================== Token Exchange ====================

// ExchangeCode exchanges an authorization code for tokens using PKCE.
func ExchangeCode(code, codeVerifier string) (*TokenResponse, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {codeVerifier},
	}
	return doTokenRequest(data)
}

// RefreshTokens exchanges a refresh_token for a new access_token.
//
// IMPORTANT: unlike the authorization_code exchange (which is form-urlencoded),
// the Codex CLI sends the refresh request as a JSON body with the reduced
// scope "openid profile email" (no offline_access). Sending it form-encoded or
// with the wider scope makes auth.openai.com reject it with HTTP 401. This
// mirrors the canonical Codex CLI behavior (see codex-lb refresh.py).
func RefreshTokens(refreshToken string) (*TokenResponse, error) {
	payload := map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     clientID,
		"refresh_token": refreshToken,
		"scope":         "openid profile email",
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		tr, err := doTokenRequestJSON(payload)
		if err == nil && tr != nil {
			return tr, nil
		}
		if err != nil {
			lastErr = err
			// invalid_grant / reused tokens are permanent — don't retry.
			if strings.Contains(err.Error(), "invalid_grant") ||
				strings.Contains(err.Error(), "reused") ||
				strings.Contains(err.Error(), "revoked") {
				return nil, err
			}
		}
		logger.Debugf("[CodexAuth] token refresh attempt %d failed: %v", attempt+1, err)
		if attempt < 2 {
			time.Sleep(time.Duration(1<<attempt) * time.Second)
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("codex token refresh failed after 3 attempts: %w", lastErr)
	}
	return nil, fmt.Errorf("codex token refresh failed after 3 attempts")
}

// doTokenRequest sends a token request and returns the parsed response.
func doTokenRequest(data url.Values) (*TokenResponse, error) {
	return doTokenRequestRaw(data)
}

func doTokenRequestRaw(data url.Values) (*TokenResponse, error) {
	resp, err := httpPostForm(tokenURL, data)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io_ReadAll(resp.Body)

	if resp.StatusCode == 200 {
		var tr TokenResponse
		if err := json.Unmarshal(respBody, &tr); err != nil {
			return nil, fmt.Errorf("codex token decode failed: %w", err)
		}
		return &tr, nil
	}

	return nil, tokenErrorFromBody(resp.StatusCode, respBody)
}

// parseTokenError extracts the OAuth error code and description from a token
// endpoint error body. OpenAI returns two shapes depending on the endpoint:
//   - flat OAuth2:  {"error":"invalid_grant","error_description":"..."}
//   - nested:       {"error":{"code":"refresh_token_reused","message":"..."}}
//
// Both must be handled, otherwise reused/revoked tokens slip past the retry
// short-circuit (see RefreshTokens).
func parseTokenError(body []byte) (code, message string) {
	var probe struct {
		Error            json.RawMessage `json:"error"`
		ErrorDescription string          `json:"error_description"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", ""
	}

	// Flat shape: error is a JSON string.
	var flat string
	if json.Unmarshal(probe.Error, &flat) == nil && flat != "" {
		return flat, probe.ErrorDescription
	}

	// Nested shape: error is an object.
	var nested struct {
		Code    string `json:"code"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(probe.Error, &nested) == nil {
		c := nested.Code
		if c == "" {
			c = nested.Error
		}
		return c, nested.Message
	}

	return "", probe.ErrorDescription
}

// tokenErrorFromBody builds the error returned for a non-200 token response,
// special-casing reused/revoked refresh tokens so callers can short-circuit.
func tokenErrorFromBody(statusCode int, body []byte) error {
	code, message := parseTokenError(body)

	if code == "refresh_token_reused" {
		return fmt.Errorf("codex refresh token reused/revoked: %s", message)
	}
	if code != "" {
		return fmt.Errorf("codex token request failed: HTTP %d %s: %s", statusCode, code, message)
	}
	// No recognizable error JSON — surface the raw body so geo-blocks / HTML
	// error pages / unexpected shapes are visible instead of a bare status.
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 300 {
		snippet = snippet[:300]
	}
	return fmt.Errorf("codex token request failed: HTTP %d body=%q", statusCode, snippet)
}

// doTokenRequestJSON sends a token request with a JSON body. Used for the
// refresh_token grant: the Codex CLI sends refreshes as JSON (not form-encoded)
// with the reduced scope "openid profile email". Sending it form-encoded or with
// offline_access makes auth.openai.com reject it with HTTP 401.
func doTokenRequestJSON(payload map[string]string) (*TokenResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("codex token request marshal failed: %w", err)
	}

	req, err := http.NewRequest("POST", tokenURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := defaultHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io_ReadAll(resp.Body)

	if resp.StatusCode == 200 {
		var tr TokenResponse
		if err := json.Unmarshal(respBody, &tr); err != nil {
			return nil, fmt.Errorf("codex token decode failed: %w", err)
		}
		return &tr, nil
	}

	return nil, tokenErrorFromBody(resp.StatusCode, respBody)
}

// ==================== Device Code Flow ====================

// StartDeviceCodeFlow initiates a device code authorization flow.
func StartDeviceCodeFlow() (*DeviceCodeResponse, error) {
	body, _ := json.Marshal(map[string]string{"client_id": clientID})
	req, err := http.NewRequest("POST", deviceCodeURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := defaultHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("device code start failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io_ReadAll(resp.Body)
		return nil, fmt.Errorf("device code start failed: HTTP %d %s", resp.StatusCode, string(errBody))
	}

	var dcr DeviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&dcr); err != nil {
		return nil, fmt.Errorf("device code start decode failed: %w", err)
	}
	if dcr.Interval == 0 {
		dcr.Interval = 5
	}
	return &dcr, nil
}

// DeviceCodePollResult is the result of a device code poll.
type DeviceCodePollResult struct {
	Completed bool
	TR        *TokenResponse
}

// PollDeviceCode polls the device code token endpoint once.
// Returns completed=true with tokens if the user has authorized.
// Returns completed=false if still pending.
func PollDeviceCode(deviceAuthID, userCode string) (*DeviceCodePollResult, error) {
	body, _ := json.Marshal(map[string]string{
		"device_auth_id": deviceAuthID,
		"user_code":      userCode,
	})
	req, err := http.NewRequest("POST", deviceTokenURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := defaultHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("device code poll failed: %w", err)
	}
	defer resp.Body.Close()

	// 200-299: success, we got the authorization code
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var result struct {
			AuthorizationCode string `json:"authorization_code"`
			CodeVerifier      string `json:"code_verifier"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, fmt.Errorf("device code poll decode failed: %w", err)
		}

		// Exchange the authorization code for tokens using device redirect URI
		data := url.Values{
			"grant_type":    {"authorization_code"},
			"client_id":     {clientID},
			"code":          {result.AuthorizationCode},
			"redirect_uri":  {deviceRedirectURI},
			"code_verifier": {result.CodeVerifier},
		}
		tr, err := doTokenRequest(data)
		if err != nil {
			return nil, err
		}
		return &DeviceCodePollResult{Completed: true, TR: tr}, nil
	}

	// 403/404: still pending
	if resp.StatusCode == 403 || resp.StatusCode == 404 {
		return &DeviceCodePollResult{Completed: false}, nil
	}

	errBody, _ := io_ReadAll(resp.Body)
	return nil, fmt.Errorf("device code poll error: HTTP %d %s", resp.StatusCode, string(errBody))
}

// ==================== Callback URL Parsing ====================

// ParseCallbackURL extracts code and state from an OAuth callback URL.
func ParseCallbackURL(callbackURL string) (code, state string, err error) {
	u, err := url.Parse(callbackURL)
	if err != nil {
		return "", "", fmt.Errorf("invalid callback URL: %w", err)
	}
	q := u.Query()
	code = q.Get("code")
	state = q.Get("state")
	if code == "" {
		return "", "", fmt.Errorf("callback URL missing code parameter")
	}
	if state == "" {
		return "", "", fmt.Errorf("callback URL missing state parameter")
	}
	return code, state, nil
}

// ==================== JWT Parsing ====================

// ExtractAccountID parses the JWT id_token to extract the ChatGPT account ID.
func ExtractAccountID(idToken string) (string, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid JWT format")
	}
	payload, err := base64urlDecode(parts[1])
	if err != nil {
		return "", fmt.Errorf("JWT payload decode failed: %w", err)
	}
	var claims struct {
		Email    string `json:"email"`
		AuthInfo struct {
			ChatGptAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("JWT claims parse failed: %w", err)
	}
	return claims.AuthInfo.ChatGptAccountID, nil
}

// ExtractEmail parses the JWT id_token to extract the email.
func ExtractEmail(idToken string) (string, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid JWT format")
	}
	payload, err := base64urlDecode(parts[1])
	if err != nil {
		return "", fmt.Errorf("JWT payload decode failed: %w", err)
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("JWT claims parse failed: %w", err)
	}
	return claims.Email, nil
}

// ==================== Helpers ====================

func httpPostForm(tokenURL string, data url.Values) (*http.Response, error) {
	req, err := http.NewRequest("POST", tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return defaultHTTPClient().Do(req)
}

func base64urlEncode(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func base64urlDecode(s string) ([]byte, error) {
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	return base64.URLEncoding.DecodeString(s)
}

// io_ReadAll is a simple wrapper to avoid importing io in some paths.
func io_ReadAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var buf []byte
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
	}
}

// GetDeviceVerifyURL returns the URL where users verify a device code.
func GetDeviceVerifyURL() string {
	return deviceVerifyURL
}
