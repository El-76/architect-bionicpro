package main

import (
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	mu sync.Mutex

	UserID                string
	Username              string
	AccessToken           string
	EncryptedRefreshToken string
	AccessTokenExpiresAt  time.Time
	LastActivity          time.Time
	AbsoluteExpiresAt     time.Time
}

type AuthRequest struct {
	Nonce        string
	CodeVerifier string
	BrowserID    string
	CreatedAt    time.Time
}

type rotatedSessionAlias struct {
	NewSessionID string
	ExpiresAt    time.Time
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
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

type internalSessionResponse struct {
	UserID      string    `json:"user_id"`
	Username    string    `json:"username,omitempty"`
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"access_token_expires_at"`
}

const (
	idleSessionTTL         = 30 * time.Minute
	absoluteSessionTTL     = 8 * time.Hour
	authRequestTTL         = 10 * time.Minute
	accessTokenRefreshSkew = 10 * time.Second
	rotationGracePeriod    = 5 * time.Second
	authBrowserCookie      = "oauth_browser"
	sessionCookie          = "session_id"

	oidcIssuer           = "http://192.168.56.106:8080/realms/reports-realm"
	oidcAuthorizationURL = oidcIssuer + "/protocol/openid-connect/auth"
	oidcTokenURL         = oidcIssuer + "/protocol/openid-connect/token"
	oidcJWKSURL          = oidcIssuer + "/protocol/openid-connect/certs"
	oidcClientID         = "bionicpro-auth"
	oidcRedirectURI      = "http://192.168.56.106:8081/auth/callback"
	frontendRedirectURI  = "http://192.168.56.106:3000/"
	frontendOrigin       = "http://192.168.56.106:3000"
)

var (
	sessionsMu        sync.RWMutex
	sessions          = make(map[string]*Session)
	rotatedSessionIDs = make(map[string]rotatedSessionAlias)
	authRequestsMu    sync.Mutex
	authRequests      = make(map[string]AuthRequest)
	httpClient        = &http.Client{Timeout: 10 * time.Second}
	tokenCipher       cipher.AEAD
	internalSecret    string
)

func main() {
	if err := initializeSecrets(); err != nil {
		log.Fatal(err)
	}
	if strings.TrimSpace(os.Getenv("OIDC_CLIENT_SECRET")) == "" {
		log.Fatal("OIDC_CLIENT_SECRET must be set")
	}

	go cleanupExpiredData()

	router := gin.Default()
	router.Use(CORSMiddleware())
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.GET("/auth/session", handleSession)
	router.GET("/auth/login", handleLogin)
	router.GET("/auth/callback", handleAuthCallback)

	// This endpoint is intended for reports-api only. It is not a browser API.
	router.POST("/internal/session/validate-and-rotate", handleInternalSessionValidation)

	log.Println("bionicpro-auth starting on :8081")
	if err := router.Run(":8081"); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

func initializeSecrets() error {
	key := os.Getenv("TOKEN_ENCRYPTION_KEY")
	if len([]byte(key)) < 32 {
		return errors.New("TOKEN_ENCRYPTION_KEY must contain at least 32 bytes")
	}
	keyDigest := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(keyDigest[:])
	if err != nil {
		return fmt.Errorf("initialize token encryption: %w", err)
	}
	tokenCipher, err = cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("initialize token encryption: %w", err)
	}
	internalSecret = os.Getenv("AUTH_INTERNAL_SECRET")
	if len([]byte(internalSecret)) < 32 {
		return errors.New("AUTH_INTERNAL_SECRET must contain at least 32 bytes")
	}
	return nil
}

func encryptRefreshToken(plainText string) (string, error) {
	if tokenCipher == nil {
		return "", errors.New("token encryption is not initialized")
	}
	nonce := make([]byte, tokenCipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	cipherText := tokenCipher.Seal(nonce, nonce, []byte(plainText), nil)
	return base64.RawStdEncoding.EncodeToString(cipherText), nil
}

func decryptRefreshToken(encoded string) (string, error) {
	if tokenCipher == nil {
		return "", errors.New("token encryption is not initialized")
	}
	cipherText, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	nonceSize := tokenCipher.NonceSize()
	if len(cipherText) < nonceSize {
		return "", errors.New("encrypted refresh token is truncated")
	}
	nonce, body := cipherText[:nonceSize], cipherText[nonceSize:]
	plainText, err := tokenCipher.Open(nil, nonce, body, nil)
	if err != nil {
		return "", err
	}
	return string(plainText), nil
}

func CORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", frontendOrigin)
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
	authRequests[state] = AuthRequest{Nonce: nonce, CodeVerifier: codeVerifier, BrowserID: browserID, CreatedAt: now}
	authRequestsMu.Unlock()

	setCookie(c, authBrowserCookie, browserID, int(authRequestTTL.Seconds()), "/auth")
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
		log.Printf("OIDC authorization code exchange failed: %v", err)
		c.AbortWithStatus(http.StatusBadGateway)
		return
	}
	log.Printf("Access token: %s", tokens.AccessToken)
	claims, err := validateIDToken(tokens.IDToken, request.Nonce)
	if err != nil {
		log.Printf("OIDC ID token validation failed: %v", err)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	userID, ok := claims["sub"].(string)
	if !ok || userID == "" || tokens.AccessToken == "" || tokens.RefreshToken == "" || tokens.ExpiresIn <= 0 {
		c.AbortWithStatus(http.StatusBadGateway)
		return
	}
	username, _ := claims["preferred_username"].(string)
	encryptedRefreshToken, err := encryptRefreshToken(tokens.RefreshToken)
	if err != nil {
		log.Printf("Could not encrypt refresh token: %v", err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	sessionID, err := generateRandomToken(32)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	now := time.Now()
	session := &Session{
		UserID: userID, Username: username, AccessToken: tokens.AccessToken,
		EncryptedRefreshToken: encryptedRefreshToken,
		AccessTokenExpiresAt:  now.Add(time.Duration(tokens.ExpiresIn) * time.Second),
		LastActivity:          now, AbsoluteExpiresAt: now.Add(absoluteSessionTTL),
	}
	sessionsMu.Lock()
	sessions[sessionID] = session
	sessionsMu.Unlock()

	setSessionCookie(c, sessionID, sessionCookieMaxAge(session, now))
	redirectFrontend(c, "", "")
}

func exchangeAuthorizationCode(ctx context.Context, code, codeVerifier string) (tokenResponse, error) {
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"code_verifier": {codeVerifier}, "client_id": {oidcClientID}, "redirect_uri": {oidcRedirectURI},
		"client_secret": {os.Getenv("OIDC_CLIENT_SECRET")},
	}
	return requestTokenEndpoint(ctx, form)
}

func refreshAccessToken(ctx context.Context, refreshToken string) (tokenResponse, error) {
	form := url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refreshToken},
		"client_id": {oidcClientID}, "client_secret": {os.Getenv("OIDC_CLIENT_SECRET")},
	}
	return requestTokenEndpoint(ctx, form)
}

func requestTokenEndpoint(ctx context.Context, form url.Values) (tokenResponse, error) {
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
	var tokens tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tokens); err != nil {
		return tokenResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		if tokens.Error != "" {
			return tokenResponse{}, fmt.Errorf("token endpoint error %s: %s", tokens.Error, tokens.Description)
		}
		return tokenResponse{}, fmt.Errorf("token endpoint returned %s", resp.Status)
	}
	if tokens.AccessToken == "" || tokens.ExpiresIn <= 0 {
		return tokenResponse{}, errors.New("token endpoint response is missing access_token or expires_in")
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
	if claims["iss"] != oidcIssuer || !hasAudience(claims["aud"], oidcClientID) {
		return nil, errors.New("ID token issuer or audience mismatch")
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
		return nil, fmt.Errorf("JWKS endpoint returned %s", resp.Status)
	}
	var keys jwksResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&keys); err != nil {
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

func handleSession(c *gin.Context) {
	sessionID, err := c.Cookie(sessionCookie)
	if err != nil || sessionID == "" {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	currentSessionID, session, _, ok := resolveSessionID(sessionID)
	if !ok {
		clearSessionCookie(c)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	sessionsMu.RLock()
	stillCurrent := sessions[currentSessionID] == session
	sessionsMu.RUnlock()
	if !stillCurrent || sessionExpired(session, time.Now()) {
		deleteSessionIfMatches(currentSessionID, session)
		clearSessionCookie(c)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	session.LastActivity = time.Now()
	setSessionCookie(c, currentSessionID, sessionCookieMaxAge(session, session.LastActivity))
	c.JSON(http.StatusOK, gin.H{
		"authenticated": true,
		"user":          gin.H{"preferred_username": session.Username},
	})
}

// reports-api calls this endpoint for each protected request. The endpoint
// rotates the session and returns the access token only over the internal hop.
func handleInternalSessionValidation(c *gin.Context) {
	providedSecret := c.GetHeader("X-Internal-Auth")
	if providedSecret == "" || subtle.ConstantTimeCompare([]byte(providedSecret), []byte(internalSecret)) != 1 {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}
	sessionID, err := c.Cookie(sessionCookie)
	if err != nil || sessionID == "" {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	newSessionID, response, cookieMaxAge, status, err := validateRefreshAndRotate(c.Request.Context(), sessionID)
	if err != nil {
		log.Printf("Internal session validation failed: %v", err)
		if status == http.StatusUnauthorized {
			clearSessionCookie(c)
		}
		c.AbortWithStatus(status)
		return
	}
	setSessionCookie(c, newSessionID, cookieMaxAge)
	c.JSON(http.StatusOK, response)
}

func validateRefreshAndRotate(ctx context.Context, oldSessionID string) (string, internalSessionResponse, int, int, error) {
	currentSessionID, session, wasAlreadyRotated, ok := resolveSessionID(oldSessionID)
	if !ok {
		return "", internalSessionResponse{}, 0, http.StatusUnauthorized, errors.New("session not found")
	}
	session.mu.Lock()
	defer session.mu.Unlock()

	// Resolve again after taking the session lock: another in-flight request
	// may have rotated the ID while this request was waiting.
	latestID, latestSession, latestWasRotated, stillCurrent := resolveSessionID(oldSessionID)
	if !stillCurrent || latestSession != session {
		return "", internalSessionResponse{}, 0, http.StatusUnauthorized, errors.New("session ID has already been rotated")
	}
	currentSessionID = latestID
	wasAlreadyRotated = wasAlreadyRotated || latestWasRotated

	now := time.Now()
	if sessionExpired(session, now) {
		deleteSessionIfMatches(currentSessionID, session)
		return "", internalSessionResponse{}, 0, http.StatusUnauthorized, errors.New("session expired")
	}

	if !session.AccessTokenExpiresAt.After(now.Add(accessTokenRefreshSkew)) {
		refreshToken, err := decryptRefreshToken(session.EncryptedRefreshToken)
		if err != nil {
			return "", internalSessionResponse{}, 0, http.StatusInternalServerError, fmt.Errorf("decrypt refresh token: %w", err)
		}
		tokens, err := refreshAccessToken(ctx, refreshToken)
		if err != nil {
			if strings.Contains(err.Error(), "invalid_grant") {
				deleteSessionIfMatches(currentSessionID, session)
				return "", internalSessionResponse{}, 0, http.StatusUnauthorized, errors.New("refresh token is no longer valid")
			}
			return "", internalSessionResponse{}, 0, http.StatusBadGateway, fmt.Errorf("refresh token request failed: %w", err)
		}
		if tokens.RefreshToken == "" {
			return "", internalSessionResponse{}, 0, http.StatusBadGateway, errors.New("Keycloak did not return a rotated refresh token")
		}
		encryptedRefreshToken, err := encryptRefreshToken(tokens.RefreshToken)
		if err != nil {
			return "", internalSessionResponse{}, 0, http.StatusInternalServerError, fmt.Errorf("encrypt rotated refresh token: %w", err)
		}
		session.AccessToken = tokens.AccessToken
		session.EncryptedRefreshToken = encryptedRefreshToken
		session.AccessTokenExpiresAt = now.Add(time.Duration(tokens.ExpiresIn) * time.Second)
	}

	session.LastActivity = now
	resultSessionID := currentSessionID
	if !wasAlreadyRotated {
		newSessionID, err := generateRandomToken(32)
		if err != nil {
			return "", internalSessionResponse{}, 0, http.StatusInternalServerError, err
		}
		sessionsMu.Lock()
		if sessions[currentSessionID] != session {
			sessionsMu.Unlock()
			return "", internalSessionResponse{}, 0, http.StatusUnauthorized, errors.New("session ID has already been rotated")
		}
		delete(sessions, currentSessionID)
		sessions[newSessionID] = session
		rotatedSessionIDs[currentSessionID] = rotatedSessionAlias{
			NewSessionID: newSessionID,
			ExpiresAt:    now.Add(rotationGracePeriod),
		}
		sessionsMu.Unlock()
		resultSessionID = newSessionID
	}
	response := internalSessionResponse{
		UserID: session.UserID, Username: session.Username,
		AccessToken: session.AccessToken, ExpiresAt: session.AccessTokenExpiresAt,
	}
	cookieMaxAge := sessionCookieMaxAge(session, now)
	return resultSessionID, response, cookieMaxAge, http.StatusOK, nil
}

func resolveSessionID(sessionID string) (string, *Session, bool, bool) {
	sessionsMu.RLock()
	defer sessionsMu.RUnlock()
	currentID := sessionID
	wasRotated := false
	now := time.Now()
	for i := 0; i < 8; i++ {
		if session, ok := sessions[currentID]; ok {
			return currentID, session, wasRotated, true
		}
		alias, ok := rotatedSessionIDs[currentID]
		if !ok || !now.Before(alias.ExpiresAt) {
			return "", nil, false, false
		}
		currentID = alias.NewSessionID
		wasRotated = true
	}
	return "", nil, false, false
}

func sessionExpired(session *Session, now time.Time) bool {
	return !now.Before(session.LastActivity.Add(idleSessionTTL)) || !now.Before(session.AbsoluteExpiresAt)
}

func sessionCookieMaxAge(session *Session, now time.Time) int {
	idleRemaining := session.LastActivity.Add(idleSessionTTL).Sub(now)
	absoluteRemaining := session.AbsoluteExpiresAt.Sub(now)
	remaining := idleRemaining
	if absoluteRemaining < remaining {
		remaining = absoluteRemaining
	}
	if remaining < time.Second {
		return 0
	}
	return int(remaining.Seconds())
}

func deleteSessionIfMatches(sessionID string, session *Session) {
	sessionsMu.Lock()
	if sessions[sessionID] == session {
		delete(sessions, sessionID)
	}
	sessionsMu.Unlock()
}

func cleanupExpiredData() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		sessionsMu.RLock()
		snapshot := make(map[string]*Session, len(sessions))
		for id, session := range sessions {
			snapshot[id] = session
		}
		sessionsMu.RUnlock()
		for id, session := range snapshot {
			session.mu.Lock()
			expired := sessionExpired(session, now)
			session.mu.Unlock()
			if expired {
				deleteSessionIfMatches(id, session)
			}
		}
		sessionsMu.Lock()
		for oldID, alias := range rotatedSessionIDs {
			if !now.Before(alias.ExpiresAt) {
				delete(rotatedSessionIDs, oldID)
			}
		}
		sessionsMu.Unlock()
		authRequestsMu.Lock()
		for state, request := range authRequests {
			if now.Sub(request.CreatedAt) >= authRequestTTL {
				delete(authRequests, state)
			}
		}
		authRequestsMu.Unlock()
	}
}

func setCookie(c *gin.Context, name, value string, maxAge int, path string) {
	c.SetSameSite(http.SameSiteLaxMode)
	// Secure=false is intentionally retained for the local HTTP test setup.
	c.SetCookie(name, value, maxAge, path, "", false, true)
}

func setSessionCookie(c *gin.Context, sessionID string, maxAge int) {
	setCookie(c, sessionCookie, sessionID, maxAge, "/")
}

func clearOAuthBrowserCookie(c *gin.Context) {
	setCookie(c, authBrowserCookie, "", -1, "/auth")
}

func clearSessionCookie(c *gin.Context) {
	setCookie(c, sessionCookie, "", -1, "/")
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
