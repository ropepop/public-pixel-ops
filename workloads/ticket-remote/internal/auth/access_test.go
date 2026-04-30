package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestValidatorAcceptsCloudflareAccessJWT(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	kid := "test-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON := map[string]any{
			"keys": []map[string]any{{
				"kid": kid,
				"kty": "RSA",
				"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}},
		}
		_ = json.NewEncoder(w).Encode(writeJSON)
	}))
	defer server.Close()

	token := signTestJWT(t, key, kid, map[string]any{
		"iss":   server.URL,
		"aud":   []string{"audience-a"},
		"email": "Member@Example.com",
		"iat":   time.Now().Add(-time.Minute).Unix(),
		"nbf":   time.Now().Add(-time.Minute).Unix(),
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	validator := NewValidator(AccessConfig{
		Mode:       "cloudflare",
		TeamDomain: server.URL,
		Audience:   "audience-a",
	})
	identity, err := validator.ValidateJWT(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Email != "member@example.com" {
		t.Fatalf("email = %q", identity.Email)
	}
}

func TestValidatorRejectsAudienceMismatch(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	kid := "test-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kid": kid,
				"kty": "RSA",
				"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}},
		})
	}))
	defer server.Close()
	token := signTestJWT(t, key, kid, map[string]any{
		"iss":   server.URL,
		"aud":   []string{"other"},
		"email": "member@example.com",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	validator := NewValidator(AccessConfig{Mode: "cloudflare", TeamDomain: server.URL, Audience: "wanted"})
	if _, err := validator.ValidateJWT(context.Background(), token); err == nil {
		t.Fatal("expected audience mismatch")
	}
}

func signTestJWT(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	headerRaw, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": kid})
	claimsRaw, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(headerRaw) + "." + base64.RawURLEncoding.EncodeToString(claimsRaw)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}
