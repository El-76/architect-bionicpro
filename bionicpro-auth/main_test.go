package main

import (
	"context"
	"encoding/base64"
	"testing"
	"time"
)

func TestRefreshTokenEncryptionRoundTrip(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", "unit-test-encryption-key-that-is-long-enough-32bytes")
	t.Setenv("AUTH_INTERNAL_SECRET", "unit-test-internal-secret-that-is-long-enough-32bytes")
	if err := initializeSecrets(); err != nil {
		t.Fatal(err)
	}

	const token = "refresh-token-value"
	encrypted, err := encryptRefreshToken(token)
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := decryptRefreshToken(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if decrypted != token {
		t.Fatalf("got %q, want %q", decrypted, token)
	}
}

func TestRefreshTokenEncryptionDetectsTampering(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", "unit-test-encryption-key-that-is-long-enough-32bytes")
	t.Setenv("AUTH_INTERNAL_SECRET", "unit-test-internal-secret-that-is-long-enough-32bytes")
	if err := initializeSecrets(); err != nil {
		t.Fatal(err)
	}

	encrypted, err := encryptRefreshToken("refresh-token-value")
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.RawStdEncoding.DecodeString(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if _, err := decryptRefreshToken(base64.RawStdEncoding.EncodeToString(data)); err == nil {
		t.Fatal("expected authentication failure after ciphertext tampering")
	}
}

func TestSessionIdleAndAbsoluteExpiration(t *testing.T) {
	now := time.Now()
	session := &Session{
		LastActivity:      now.Add(-idleSessionTTL - time.Second),
		AbsoluteExpiresAt: now.Add(time.Hour),
	}
	if !sessionExpired(session, now) {
		t.Fatal("expected idle-expired session")
	}

	session.LastActivity = now
	session.AbsoluteExpiresAt = now.Add(-time.Second)
	if !sessionExpired(session, now) {
		t.Fatal("expected absolute-expired session")
	}
}

func TestSessionCookieMaxAgeUsesEarlierDeadline(t *testing.T) {
	now := time.Now()
	session := &Session{
		LastActivity:      now,
		AbsoluteExpiresAt: now.Add(5 * time.Minute),
	}
	maxAge := sessionCookieMaxAge(session, now)
	if maxAge > 5*60 || maxAge < 5*60-1 {
		t.Fatalf("max age should be bounded by absolute expiry, got %d", maxAge)
	}
}

func TestEncryptionKeyIsRequired(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", "")
	if err := initializeSecrets(); err == nil {
		t.Fatal("expected missing encryption key to fail")
	}
}

func TestSessionRotationAndConcurrentRequestGrace(t *testing.T) {
	now := time.Now()
	session := &Session{
		UserID:               "user-1",
		AccessToken:          "access-token",
		AccessTokenExpiresAt: now.Add(time.Minute),
		LastActivity:         now,
		AbsoluteExpiresAt:    now.Add(absoluteSessionTTL),
	}
	oldID := "old-session-id"
	sessionsMu.Lock()
	sessions = map[string]*Session{oldID: session}
	rotatedSessionIDs = make(map[string]rotatedSessionAlias)
	sessionsMu.Unlock()
	t.Cleanup(func() {
		sessionsMu.Lock()
		sessions = make(map[string]*Session)
		rotatedSessionIDs = make(map[string]rotatedSessionAlias)
		sessionsMu.Unlock()
	})

	newID, response, _, status, err := validateRefreshAndRotate(context.Background(), oldID)
	if err != nil {
		t.Fatalf("first validation failed (status %d): %v", status, err)
	}
	if newID == oldID || response.AccessToken != "access-token" {
		t.Fatal("expected a new session ID and the server-side access token")
	}

	graceID, _, _, status, err := validateRefreshAndRotate(context.Background(), oldID)
	if err != nil {
		t.Fatalf("concurrent validation grace failed (status %d): %v", status, err)
	}
	if graceID != newID {
		t.Fatalf("expected concurrent request to reuse rotated ID %q, got %q", newID, graceID)
	}
}
