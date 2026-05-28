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
func RefreshTokens(refreshToken string) (*TokenResponse, error) {
	data := url.Values{
		"client_id":     {clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {scopeStr},
	}

	for attempt := 0; attempt < 3; attempt++ {
		tr, err := doTokenRequestRaw(data)
		if err != nil {
			return nil, fmt.Errorf("codex token refresh request failed: %w", err)
		}
		if tr != nil {
			return tr, nil
		}
		logger.Debugf("[CodexAuth] token refresh attempt %d failed", attempt+1)
		if attempt < 2 {
			time.Sleep(time.Duration(1<<attempt) * time.Second)
		}
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

	if resp.StatusCode == 200 {
		var tr TokenResponse
		if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
			return nil, fmt.Errorf("codex token decode failed: %w", err)
		}
		return &tr, nil
	}

	var errBody struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&errBody)

	if errBody.Error == "refresh_token_reused" {
		return nil, fmt.Errorf("codex refresh token reused/revoked: %s", errBody.ErrorDescription)
	}
	return nil, fmt.Errorf("codex token request failed: HTTP %d %s", resp.StatusCode, errBody.Error)
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

	resp, err := defaultHTTPClient.Do(req)
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

	resp, err := defaultHTTPClient.Do(req)
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
	return defaultHTTPClient.Do(req)
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
