package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

type AccessConfig struct {
	Mode        string
	TeamDomain  string
	Audience    string
	DevEmail    string
	HTTPTimeout time.Duration
}

type Identity struct {
	Email string `json:"email"`
}

type Validator struct {
	cfg    AccessConfig
	client *http.Client

	mu         sync.Mutex
	cachedAt   time.Time
	cachedKeys map[string]*rsa.PublicKey
}

func NewValidator(cfg AccessConfig) *Validator {
	timeout := cfg.HTTPTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Validator{
		cfg:    cfg,
		client: &http.Client{Timeout: timeout},
	}
}

func (v *Validator) IdentityFromRequest(ctx context.Context, r *http.Request) (Identity, error) {
	mode := strings.ToLower(strings.TrimSpace(v.cfg.Mode))
	if mode == "" {
		mode = "cloudflare"
	}
	if mode == "dev" || mode == "development" {
		email := normalizeEmail(r.Header.Get("X-Ticket-Remote-Email"))
		if email == "" {
			email = normalizeEmail(r.URL.Query().Get("email"))
		}
		if email == "" {
			email = normalizeEmail(v.cfg.DevEmail)
		}
		if email == "" {
			return Identity{}, errors.New("dev email is not configured")
		}
		return Identity{Email: email}, nil
	}
	if mode == "none" {
		email := normalizeEmail(v.cfg.DevEmail)
		if email == "" {
			return Identity{}, errors.New("auth disabled but no dev email configured")
		}
		return Identity{Email: email}, nil
	}

	token := strings.TrimSpace(r.Header.Get("Cf-Access-Jwt-Assertion"))
	if token == "" {
		return Identity{}, errors.New("missing Cloudflare Access assertion")
	}
	return v.ValidateJWT(ctx, token)
}

func (v *Validator) ValidateJWT(ctx context.Context, token string) (Identity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, errors.New("invalid JWT shape")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeJWTPart(parts[0], &header); err != nil {
		return Identity{}, fmt.Errorf("decode JWT header: %w", err)
	}
	if header.Alg != "RS256" {
		return Identity{}, fmt.Errorf("unsupported JWT algorithm %q", header.Alg)
	}
	if strings.TrimSpace(header.Kid) == "" {
		return Identity{}, errors.New("JWT key id is missing")
	}
	keys, err := v.keys(ctx)
	if err != nil {
		return Identity{}, err
	}
	key := keys[header.Kid]
	if key == nil {
		v.clearKeyCache()
		keys, err = v.keys(ctx)
		if err != nil {
			return Identity{}, err
		}
		key = keys[header.Kid]
	}
	if key == nil {
		return Identity{}, fmt.Errorf("Cloudflare Access key %q not found", header.Kid)
	}
	signed := []byte(parts[0] + "." + parts[1])
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Identity{}, fmt.Errorf("decode JWT signature: %w", err)
	}
	digest := sha256.Sum256(signed)
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return Identity{}, fmt.Errorf("verify JWT signature: %w", err)
	}

	var claims map[string]any
	if err := decodeJWTPart(parts[1], &claims); err != nil {
		return Identity{}, fmt.Errorf("decode JWT claims: %w", err)
	}
	now := time.Now().Unix()
	if exp, ok := numericClaim(claims["exp"]); ok && now >= exp {
		return Identity{}, errors.New("Cloudflare Access assertion expired")
	}
	if nbf, ok := numericClaim(claims["nbf"]); ok && now < nbf {
		return Identity{}, errors.New("Cloudflare Access assertion is not valid yet")
	}
	if iat, ok := numericClaim(claims["iat"]); ok && now+120 < iat {
		return Identity{}, errors.New("Cloudflare Access assertion issued in the future")
	}
	if expected := strings.TrimSpace(v.cfg.Audience); expected != "" && !claimContains(claims["aud"], expected) {
		return Identity{}, errors.New("Cloudflare Access audience mismatch")
	}
	if teamDomain := strings.TrimRight(strings.TrimSpace(v.cfg.TeamDomain), "/"); teamDomain != "" {
		expectedIssuer := teamDomain
		if !strings.HasPrefix(expectedIssuer, "https://") && !strings.HasPrefix(expectedIssuer, "http://") {
			expectedIssuer = "https://" + expectedIssuer
		}
		if issuer := strings.TrimSpace(stringClaim(claims["iss"])); issuer != "" && issuer != expectedIssuer {
			return Identity{}, errors.New("Cloudflare Access issuer mismatch")
		}
	}
	email := normalizeEmail(stringClaim(claims["email"]))
	if email == "" {
		email = normalizeEmail(stringClaim(claims["common_name"]))
	}
	if email == "" {
		email = normalizeEmail(stringClaim(claims["sub"]))
	}
	if email == "" || !strings.Contains(email, "@") {
		return Identity{}, errors.New("Cloudflare Access email claim missing")
	}
	return Identity{Email: email}, nil
}

func (v *Validator) keys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.cachedKeys != nil && time.Since(v.cachedAt) < 6*time.Hour {
		return v.cachedKeys, nil
	}
	teamDomain := strings.TrimRight(strings.TrimSpace(v.cfg.TeamDomain), "/")
	if teamDomain == "" {
		return nil, errors.New("TICKET_REMOTE_CF_ACCESS_TEAM_DOMAIN is required")
	}
	if !strings.HasPrefix(teamDomain, "https://") && !strings.HasPrefix(teamDomain, "http://") {
		teamDomain = "https://" + teamDomain
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, teamDomain+"/cdn-cgi/access/certs", nil)
	if err != nil {
		return nil, fmt.Errorf("build Cloudflare certs request: %w", err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch Cloudflare Access certs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch Cloudflare Access certs: http %d", resp.StatusCode)
	}
	var payload struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode Cloudflare Access certs: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(payload.Keys))
	for _, raw := range payload.Keys {
		key, err := raw.rsaPublicKey()
		if err != nil || raw.Kid == "" {
			continue
		}
		keys[raw.Kid] = key
	}
	if len(keys) == 0 {
		return nil, errors.New("Cloudflare Access certs response had no usable RSA keys")
	}
	v.cachedAt = time.Now()
	v.cachedKeys = keys
	return keys, nil
}

func (v *Validator) clearKeyCache() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.cachedAt = time.Time{}
	v.cachedKeys = nil
}

type jwk struct {
	Kid string   `json:"kid"`
	Kty string   `json:"kty"`
	Use string   `json:"use"`
	N   string   `json:"n"`
	E   string   `json:"e"`
	X5c []string `json:"x5c"`
}

func (j jwk) rsaPublicKey() (*rsa.PublicKey, error) {
	if len(j.X5c) > 0 {
		raw, err := base64.StdEncoding.DecodeString(j.X5c[0])
		if err == nil {
			cert, certErr := x509.ParseCertificate(raw)
			if certErr == nil {
				if key, ok := cert.PublicKey.(*rsa.PublicKey); ok {
					return key, nil
				}
			}
		}
	}
	if strings.ToUpper(j.Kty) != "RSA" {
		return nil, errors.New("not an RSA key")
	}
	nBytes, err := base64.RawURLEncoding.DecodeString(j.N)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(j.E)
	if err != nil {
		return nil, err
	}
	e := 0
	for _, b := range eBytes {
		e = e<<8 + int(b)
	}
	if e == 0 {
		return nil, errors.New("invalid RSA exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
}

func decodeJWTPart(part string, target any) error {
	raw, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func claimContains(value any, expected string) bool {
	switch typed := value.(type) {
	case string:
		return typed == expected
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok && text == expected {
				return true
			}
		}
	}
	return false
}

func numericClaim(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case json.Number:
		n, err := typed.Int64()
		return n, err == nil
	}
	return 0, false
}

func stringClaim(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func normalizeEmail(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
