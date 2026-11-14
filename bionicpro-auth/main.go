package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

type Session struct {
	UserID       string
	AccessToken  string
	RefreshToken string
	UpdatedAt    time.Time
	ExpiresAt    time.Time
}

type AuthRequest struct {
	Nonce        string
	CodeVerifier string
	BrowserID    string
	CreatedAt    time.Time
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

type jwksResponse struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

const (
	sessionTTL        = 30 * time.Minute
	authRequestTTL    = 10 * time.Minute
	authBrowserCookie = "oauth_browser"
	sessionCookie     = "session_id"

	oidcIssuer           = "http://192.168.56.106:8080/realms/reports-realm"
	oidcAuthorizationURL = oidcIssuer + "/protocol/openid-connect/auth"
	oidcTokenURL         = oidcIssuer + "/protocol/openid-connect/token"
	oidcJWKSURL          = oidcIssuer + "/protocol/openid-connect/certs"
	oidcClientID         = "bionicpro-auth"
	oidcClientSecretEnv  = "OIDC_CLIENT_SECRET"
	oidcRedirectURI      = "http://192.168.56.106:8081/auth/callback"
	frontendRedirectURI  = "http://192.168.56.106:3000/"
)

var (
	sessionsMu     sync.RWMutex
	sessions       = make(map[string]Session)
	authRequestsMu sync.Mutex
	authRequests   = make(map[string]AuthRequest)
	httpClient     = &http.Client{Timeout: 10 * time.Second}
)

func CORSMiddleware() gin.HandlerFunc {
    return func(c *gin.Context) {
        c.Header("Access-Control-Allow-Origin", "http://192.168.56.106:3000")
        c.Header("Access-Control-Allow-Credentials", "true")
        c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
        c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")

        if c.Request.Method == http.MethodOptions {
            c.AbortWithStatus(http.StatusNoContent)
            return
        }

        c.Next()
    }
}

func main() {
	router := gin.Default()
        router.Use(CORSMiddleware())
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.GET("/auth/session", handleSession)
	router.GET("/auth/login", handleLogin)
	router.GET("/auth/callback", handleAuthCallback)

	log.Println("API starting on :8081")
	if err := router.Run(":8081"); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

func handleLogin(c *gin.Context) {
	state, err := generateRandomToken(32)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	nonce, err := generateRandomToken(32)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	codeVerifier, err := generateRandomToken(32)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	browserID, err := generateRandomToken(32)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	challengeDigest := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(challengeDigest[:])

	now := time.Now()
	authRequestsMu.Lock()
	for requestState, request := range authRequests {
		if now.Sub(request.CreatedAt) >= authRequestTTL {
			delete(authRequests, requestState)
		}
	}
	authRequests[state] = AuthRequest{
		Nonce:        nonce,
		CodeVerifier: codeVerifier,
		BrowserID:    browserID,
		CreatedAt:    now,
	}
	authRequestsMu.Unlock()

	c.SetSameSite(http.SameSiteLaxMode)
	// c.SetCookie(authBrowserCookie, browserID, int(authRequestTTL.Seconds()), "/auth", "", true, true)
        c.SetCookie(authBrowserCookie, browserID, int(authRequestTTL.Seconds()), "/auth", "", false, true)

	params := url.Values{
		"client_id":             {oidcClientID},
		"redirect_uri":          {oidcRedirectURI},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
		"response_mode":         {"query"},
		"response_type":         {"code"},
		"scope":                 {"openid"},
	}
	c.Redirect(http.StatusFound, oidcAuthorizationURL+"?"+params.Encode())
}

func handleAuthCallback(c *gin.Context) {
	code := c.Query("code")
	state := c.Query("state")
	oauthError := c.Query("error")
	if state == "" || (code == "" && oauthError == "") {
		clearOAuthBrowserCookie(c)
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	authRequestsMu.Lock()
	request, exists := authRequests[state]
	if exists {
		delete(authRequests, state)
	}
	authRequestsMu.Unlock()
	if !exists || time.Since(request.CreatedAt) >= authRequestTTL {
		clearOAuthBrowserCookie(c)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	browserID, err := c.Cookie(authBrowserCookie)
	if err != nil || browserID == "" || subtle.ConstantTimeCompare([]byte(browserID), []byte(request.BrowserID)) != 1 {
		clearOAuthBrowserCookie(c)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	clearOAuthBrowserCookie(c)

	if oauthError != "" {
		redirectFrontend(c, "login_error", "authentication_failed")
		return
	}

	tokens, err := exchangeAuthorizationCode(c.Request.Context(), code, request.CodeVerifier)
	if err != nil {
		log.Printf("OIDC token exchange failed: %v", err)
		c.AbortWithStatus(http.StatusBadGateway)
		return
	}
	claims, err := validateIDToken(tokens.IDToken, request.Nonce)
	if err != nil {
		log.Printf("OIDC ID token validation failed: %v", err)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	userID, ok := claims["sub"].(string)
	if !ok || userID == "" {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	if tokens.AccessToken == "" || tokens.ExpiresIn <= 0 {
		c.AbortWithStatus(http.StatusBadGateway)
		return
	}

	sessionID, err := generateRandomToken(32)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	now := time.Now()
	session := Session{
		UserID:       userID,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		UpdatedAt:    now,
		ExpiresAt:    now.Add(time.Duration(tokens.ExpiresIn) * time.Second),
	}
	sessionsMu.Lock()
	sessions[sessionID] = session
	sessionsMu.Unlock()

	c.SetSameSite(http.SameSiteLaxMode)
	// c.SetCookie(sessionCookie, sessionID, int(session.ExpiresAt.Sub(now).Seconds()), "/", "", true, true)
        c.SetCookie(sessionCookie, sessionID, int(session.ExpiresAt.Sub(now).Seconds()), "/", "", false, true)
	redirectFrontend(c, "", "")
}

func exchangeAuthorizationCode(ctx context.Context, code, codeVerifier string) (tokenResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {codeVerifier},
		"client_id":     {oidcClientID},
		"redirect_uri":  {oidcRedirectURI},
	}
	if secret := os.Getenv(oidcClientSecretEnv); secret != "" {
		form.Set("client_secret", secret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oidcTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return tokenResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return tokenResponse{}, errors.New("token endpoint returned " + resp.Status)
	}
	var tokens tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		return tokenResponse{}, err
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" || tokens.IDToken == "" {
		return tokenResponse{}, errors.New("token endpoint response is missing required tokens")
	}
	return tokens, nil
}

func validateIDToken(rawToken, expectedNonce string) (map[string]interface{}, error) {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed ID token")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("malformed ID token header")
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil || header.Algorithm != "RS256" || header.KeyID == "" {
		return nil, errors.New("unsupported ID token signing algorithm")
	}

	publicKey, err := fetchSigningKey(header.KeyID)
	if err != nil {
		return nil, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("malformed ID token signature")
	}
	signedContent := parts[0] + "." + parts[1]
	digest := sha256.Sum256([]byte(signedContent))
	if err := rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature); err != nil {
		return nil, err
	}

	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("malformed ID token claims")
	}
	var claims map[string]interface{}
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return nil, err
	}
	if claims["iss"] != oidcIssuer {
		return nil, errors.New("ID token issuer mismatch")
	}
	if !hasAudience(claims["aud"], oidcClientID) {
		return nil, errors.New("ID token audience mismatch")
	}
	expiresAt, ok := claims["exp"].(float64)
	if !ok || time.Now().Unix() >= int64(expiresAt) {
		return nil, errors.New("ID token is expired or has no expiration")
	}
	if nonce, ok := claims["nonce"].(string); !ok || subtle.ConstantTimeCompare([]byte(nonce), []byte(expectedNonce)) != 1 {
		return nil, errors.New("ID token nonce mismatch")
	}
	return claims, nil
}

func fetchSigningKey(keyID string) (*rsa.PublicKey, error) {
	req, err := http.NewRequest(http.MethodGet, oidcJWKSURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("JWKS endpoint returned " + resp.Status)
	}
	var keys jwksResponse
	if err := json.NewDecoder(resp.Body).Decode(&keys); err != nil {
		return nil, err
	}
	for _, key := range keys.Keys {
		if key.Kid != keyID || key.Kty != "RSA" || (key.Use != "" && key.Use != "sig") || (key.Alg != "" && key.Alg != "RS256") {
			continue
		}
		modulusBytes, err := base64.RawURLEncoding.DecodeString(key.N)
		if err != nil {
			return nil, err
		}
		exponentBytes, err := base64.RawURLEncoding.DecodeString(key.E)
		if err != nil {
			return nil, err
		}
		exponent := new(big.Int).SetBytes(exponentBytes)
		if !exponent.IsInt64() || exponent.Int64() < 3 {
			return nil, errors.New("invalid RSA public exponent in JWKS")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(modulusBytes), E: int(exponent.Int64())}, nil
	}
	return nil, errors.New("matching signing key not found in JWKS")
}

func hasAudience(value interface{}, expected string) bool {
	switch audience := value.(type) {
	case string:
		return audience == expected
	case []interface{}:
		for _, item := range audience {
			if item == expected {
				return true
			}
		}
	}
	return false
}

func clearOAuthBrowserCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteLaxMode)
	// c.SetCookie(authBrowserCookie, "", -1, "/auth", "", true, true)
        c.SetCookie(authBrowserCookie, "", -1, "/auth", "", false, true)
}

func redirectFrontend(c *gin.Context, key, value string) {
	destination, err := url.Parse(frontendRedirectURI)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	if key != "" {
		query := destination.Query()
		query.Set(key, value)
		destination.RawQuery = query.Encode()
	}
	c.Redirect(http.StatusFound, destination.String())
}

func generateRandomToken(byteLength int) (string, error) {
	value := make([]byte, byteLength)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func handleSession(c *gin.Context) {
	sessionID, err := c.Cookie(sessionCookie)
	if err != nil || sessionID == "" {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	sessionsMu.RLock()
	session, exists := sessions[sessionID]
	sessionsMu.RUnlock()
	if !exists || time.Since(session.UpdatedAt) >= sessionTTL || time.Now().After(session.ExpiresAt) {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	// c.SetCookie(sessionCookie, sessionID, int(time.Until(session.ExpiresAt).Seconds()), "/", "", true, true)
       c.SetCookie(sessionCookie, sessionID, int(time.Until(session.ExpiresAt).Seconds()), "/", "", false, true)
}
