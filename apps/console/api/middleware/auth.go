/*
Copyright 2025 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"k8s.io/klog/v2"
)

const (
	sessionCookieName = "aibrix_session"
	sessionMaxAge     = 24 * time.Hour

	authModeDev   = "dev"
	authModeBasic = "basic"
	authModeOIDC  = "oidc"
)

// AuthConfig holds authentication configuration.
type AuthConfig struct {
	Mode             string // "dev", "oidc", "basic"
	OIDCIssuerURL    string
	OIDCClientID     string
	OIDCClientSecret string
	OIDCRedirectURL  string
	SessionSecret    string
	DevUserName      string
	DevUserEmail     string
	BasicUsername    string
	BasicPassword    string
}

// UserInfo represents an authenticated user.
type UserInfo struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

// contextKey is an unexported type for context keys in this package.
type contextKey string

// UserContextKey is the context key for storing UserInfo.
const UserContextKey contextKey = "user"

// GetUser extracts UserInfo from the request context.
func GetUser(ctx context.Context) *UserInfo {
	if u, ok := ctx.Value(UserContextKey).(*UserInfo); ok {
		return u
	}
	return nil
}

// AuthMiddleware provides authentication for HTTP handlers.
type AuthMiddleware struct {
	config AuthConfig
}

// NewAuthMiddleware creates a new AuthMiddleware with the given configuration.
func NewAuthMiddleware(cfg AuthConfig) *AuthMiddleware {
	if cfg.Mode == "" {
		cfg.Mode = authModeDev
	}
	if cfg.SessionSecret == "" {
		cfg.SessionSecret = "aibrix-default-secret"
	}
	return &AuthMiddleware{
		config: cfg,
	}
}

// publicPaths that skip authentication checks.
var publicPaths = []string{
	"/api/v1/auth/config",
	"/api/v1/auth/login",
	"/api/v1/auth/callback",
	"/api/v1/health",
}

// isPublicPath checks whether the given path should skip authentication.
func isPublicPath(path string) bool {
	for _, p := range publicPaths {
		if path == p {
			return true
		}
	}
	return false
}

// Handler wraps an http.Handler with authentication.
func (a *AuthMiddleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		switch a.config.Mode {
		case authModeDev:
			user := &UserInfo{
				ID:    "dev-user",
				Name:  a.config.DevUserName,
				Email: a.config.DevUserEmail,
				Role:  "admin",
			}
			ctx := context.WithValue(r.Context(), UserContextKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))

		case authModeBasic, authModeOIDC:
			user, err := a.getUserFromSession(r)
			if err != nil {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), UserContextKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))

		default:
			klog.Errorf("Unknown auth mode: %s", a.config.Mode)
			http.Error(w, `{"error":"server misconfiguration"}`, http.StatusInternalServerError)
		}
	})
}

// RegisterAuthRoutes registers authentication endpoints on the grpc-gateway mux.
//
// Routes registered:
//
//	GET  /api/v1/auth/config   -> returns auth mode info (public)
//	GET  /api/v1/auth/login    -> redirect to OIDC provider (oidc mode)
//	POST /api/v1/auth/login    -> basic auth login (basic mode)
//	GET  /api/v1/auth/callback -> OIDC callback handler (oidc mode)
//	GET  /api/v1/auth/userinfo -> current user info (all modes)
//	POST /api/v1/auth/logout   -> clear session (all modes)
func (a *AuthMiddleware) RegisterAuthRoutes(mux *runtime.ServeMux) {
	_ = mux.HandlePath("GET", "/api/v1/auth/config", a.handleAuthConfig)
	_ = mux.HandlePath("GET", "/api/v1/auth/login", a.handleLoginGet)
	_ = mux.HandlePath("POST", "/api/v1/auth/login", a.handleLoginPost)
	_ = mux.HandlePath("GET", "/api/v1/auth/callback", a.handleCallback)
	_ = mux.HandlePath("GET", "/api/v1/auth/userinfo", a.handleUserInfo)
	_ = mux.HandlePath("POST", "/api/v1/auth/logout", a.handleLogout)
}

// handleAuthConfig returns the current authentication mode configuration.
func (a *AuthMiddleware) handleAuthConfig(
	w http.ResponseWriter, r *http.Request, _ map[string]string,
) {
	resp := map[string]string{
		"mode": a.config.Mode,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleLoginGet handles GET /api/v1/auth/login.
// In OIDC mode, this redirects to the OIDC provider.
// In other modes, it returns method-not-allowed or mode info.
func (a *AuthMiddleware) handleLoginGet(
	w http.ResponseWriter, r *http.Request, _ map[string]string,
) {
	switch a.config.Mode {
	case authModeOIDC:
		// TODO: Initialize OIDC provider and redirect to authorization URL.
		// When OIDC dependencies are added, this will:
		//   1. Create an oidc.Provider from OIDCIssuerURL
		//   2. Build an oauth2.Config with ClientID, ClientSecret, RedirectURL
		//   3. Generate a state parameter and store it in a cookie
		//   4. Redirect to provider.Endpoint().AuthURL with the state
		http.Error(w,
			`{"error":"oidc not yet implemented"}`,
			http.StatusNotImplemented)
	case authModeBasic:
		writeJSON(w, http.StatusOK, map[string]string{
			"mode":    authModeBasic,
			"message": "use POST with username and password",
		})
	case authModeDev:
		writeJSON(w, http.StatusOK, map[string]string{
			"mode":    authModeDev,
			"message": "no login required in dev mode",
		})
	default:
		http.Error(w, `{"error":"unknown auth mode"}`, http.StatusInternalServerError)
	}
}

// basicLoginRequest is the expected JSON body for basic auth login.
type basicLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLoginPost handles POST /api/v1/auth/login for basic auth mode.
func (a *AuthMiddleware) handleLoginPost(
	w http.ResponseWriter, r *http.Request, _ map[string]string,
) {
	if a.config.Mode != authModeBasic {
		http.Error(w,
			`{"error":"POST login only supported in basic mode"}`,
			http.StatusBadRequest)
		return
	}

	var req basicLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if req.Username != a.config.BasicUsername ||
		req.Password != a.config.BasicPassword {
		http.Error(w, `{"error":"invalid credentials"}`, http.StatusUnauthorized)
		return
	}

	user := &UserInfo{
		ID:    fmt.Sprintf("basic-%s", req.Username),
		Name:  req.Username,
		Email: fmt.Sprintf("%s@local", req.Username),
		Role:  "admin",
	}

	if err := a.setSessionCookie(w, user); err != nil {
		klog.Errorf("Failed to set session cookie: %v", err)
		http.Error(w,
			`{"error":"failed to create session"}`,
			http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(user)
}

// handleCallback handles the OIDC callback at GET /api/v1/auth/callback.
func (a *AuthMiddleware) handleCallback(
	w http.ResponseWriter, r *http.Request, _ map[string]string,
) {
	if a.config.Mode != authModeOIDC {
		http.Error(w,
			`{"error":"callback only available in oidc mode"}`,
			http.StatusBadRequest)
		return
	}

	// TODO: Implement OIDC callback when dependencies are available.
	// When implemented, this will:
	//   1. Verify the state parameter from the cookie
	//   2. Exchange the authorization code for tokens using oauth2.Config
	//   3. Validate the ID token using oidc.Verifier
	//   4. Extract claims (sub, email, name) from the ID token
	//   5. Create a UserInfo and set a session cookie
	//   6. Redirect to frontend root "/"
	http.Error(w,
		`{"error":"oidc not yet implemented"}`,
		http.StatusNotImplemented)
}

// handleUserInfo returns the current authenticated user's info.
func (a *AuthMiddleware) handleUserInfo(
	w http.ResponseWriter, r *http.Request, _ map[string]string,
) {
	var user *UserInfo

	switch a.config.Mode {
	case authModeDev:
		user = &UserInfo{
			ID:    "dev-user",
			Name:  a.config.DevUserName,
			Email: a.config.DevUserEmail,
			Role:  "admin",
		}
	case authModeBasic, authModeOIDC:
		var err error
		user, err = a.getUserFromSession(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
	default:
		http.Error(w, `{"error":"unknown auth mode"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(user)
}

// handleLogout clears the session cookie.
func (a *AuthMiddleware) handleLogout(
	w http.ResponseWriter, r *http.Request, _ map[string]string,
) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]string{
		"message": "logged out",
	})
}

// --- Session management using signed cookies ---

// setSessionCookie creates a signed session cookie containing the user info.
func (a *AuthMiddleware) setSessionCookie(w http.ResponseWriter, user *UserInfo) error {
	payload, err := json.Marshal(user)
	if err != nil {
		return fmt.Errorf("marshal user: %w", err)
	}

	sig := a.sign(payload)
	// Cookie value format: base64(payload).base64(signature)
	cookieValue := base64.RawURLEncoding.EncodeToString(payload) +
		"." +
		base64.RawURLEncoding.EncodeToString(sig)

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    cookieValue,
		Path:     "/",
		MaxAge:   int(sessionMaxAge.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// getUserFromSession validates the session cookie and returns the user info.
func (a *AuthMiddleware) getUserFromSession(r *http.Request) (*UserInfo, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil, fmt.Errorf("no session cookie: %w", err)
	}

	parts := strings.SplitN(cookie.Value, ".", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid session cookie format")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode signature: %w", err)
	}

	if !a.verify(payload, sig) {
		return nil, fmt.Errorf("invalid session signature")
	}

	var user UserInfo
	if err := json.Unmarshal(payload, &user); err != nil {
		return nil, fmt.Errorf("unmarshal user: %w", err)
	}

	return &user, nil
}

// sign computes an HMAC-SHA256 signature of the data using the session secret.
func (a *AuthMiddleware) sign(data []byte) []byte {
	mac := hmac.New(sha256.New, []byte(a.config.SessionSecret))
	mac.Write(data)
	return mac.Sum(nil)
}

// verify checks the HMAC-SHA256 signature of the data.
func (a *AuthMiddleware) verify(data, signature []byte) bool {
	expected := a.sign(data)
	return hmac.Equal(expected, signature)
}

// writeJSON is a helper to write a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, statusCode int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(v)
}
