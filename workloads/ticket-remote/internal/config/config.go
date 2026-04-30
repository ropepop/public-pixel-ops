package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"ticketremote/internal/auth"
	"ticketremote/internal/state"
)

type Config struct {
	BindAddr            string
	Port                int
	PublicBaseURL       string
	TicketID            string
	TicketDisplayName   string
	BootstrapAdminEmail string
	CookieName          string
	CookieTTL           time.Duration
	Access              auth.AccessConfig
	State               state.StoreConfig
	Phone               PhoneConfig
}

type PhoneConfig struct {
	BackendID         string
	AttachName        string
	BaseURL           string
	RequestTimeout    time.Duration
	ReconnectMinDelay time.Duration
	ReconnectMaxDelay time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		BindAddr:            getenv("TICKET_REMOTE_BIND_ADDR", "0.0.0.0"),
		Port:                getenvInt("TICKET_REMOTE_PORT", 9338),
		PublicBaseURL:       strings.TrimRight(getenv("TICKET_REMOTE_PUBLIC_BASE_URL", "https://ticket.jolkins.id.lv"), "/"),
		TicketID:            getenv("TICKET_REMOTE_TICKET_ID", "vivi-default"),
		TicketDisplayName:   getenv("TICKET_REMOTE_TICKET_DISPLAY_NAME", "ViVi timed ticket"),
		BootstrapAdminEmail: normalizeEmail(getenv("TICKET_REMOTE_BOOTSTRAP_ADMIN_EMAIL", "ticket@jolkins.id.lv")),
		CookieName:          getenv("TICKET_REMOTE_COOKIE_NAME", "ticket_remote_session"),
		CookieTTL:           getenvDuration("TICKET_REMOTE_COOKIE_TTL", 30*24*time.Hour),
		Access: auth.AccessConfig{
			Mode:       getenv("TICKET_REMOTE_AUTH_MODE", "cloudflare"),
			TeamDomain: strings.TrimRight(getenv("TICKET_REMOTE_CF_ACCESS_TEAM_DOMAIN", ""), "/"),
			Audience:   getenv("TICKET_REMOTE_CF_ACCESS_AUDIENCE", ""),
			DevEmail:   normalizeEmail(getenv("TICKET_REMOTE_DEV_EMAIL", "ticket@jolkins.id.lv")),
			HTTPTimeout: getenvDuration(
				"TICKET_REMOTE_CF_ACCESS_CERTS_TIMEOUT",
				10*time.Second,
			),
		},
		State: state.StoreConfig{
			Backend:              getenv("TICKET_REMOTE_STATE_BACKEND", "auto"),
			TicketID:             getenv("TICKET_REMOTE_TICKET_ID", "vivi-default"),
			SpacetimeHost:        strings.TrimRight(getenv("TICKET_REMOTE_SPACETIME_HOST", "https://maincloud.spacetimedb.com"), "/"),
			SpacetimeDatabase:    getenv("TICKET_REMOTE_SPACETIME_DATABASE", ""),
			SpacetimeBearerToken: getenv("TICKET_REMOTE_SPACETIME_BEARER_TOKEN", ""),
			SpacetimeIssuer:      getenv("TICKET_REMOTE_SPACETIME_OIDC_ISSUER", "ticket-remote-runtime"),
			SpacetimeAudience:    getenv("TICKET_REMOTE_SPACETIME_OIDC_AUDIENCE", "spacetimedb"),
			SpacetimeKeyFile:     getenv("TICKET_REMOTE_SPACETIME_JWT_PRIVATE_KEY_FILE", ""),
			ServiceSubject:       getenv("TICKET_REMOTE_SPACETIME_SERVICE_SUBJECT", "service:ticket-remote"),
			ServiceRoles:         splitCSV(getenv("TICKET_REMOTE_SPACETIME_SERVICE_ROLES", "ticketremote_service")),
			TokenTTL:             getenvDuration("TICKET_REMOTE_SPACETIME_TOKEN_TTL", 5*time.Minute),
			HTTPTimeout:          getenvDuration("TICKET_REMOTE_SPACETIME_HTTP_TIMEOUT", 10*time.Second),
		},
		Phone: PhoneConfig{
			BackendID:         getenv("TICKET_REMOTE_PHONE_BACKEND_ID", "pixel"),
			AttachName:        getenv("TICKET_REMOTE_PHONE_ATTACH_NAME", "Pixel"),
			BaseURL:           strings.TrimRight(getenv("TICKET_REMOTE_PHONE_BASE_URL", "http://127.0.0.1:9388"), "/"),
			RequestTimeout:    getenvDuration("TICKET_REMOTE_PHONE_REQUEST_TIMEOUT", 10*time.Second),
			ReconnectMinDelay: getenvDuration("TICKET_REMOTE_PHONE_RECONNECT_MIN_DELAY", 500*time.Millisecond),
			ReconnectMaxDelay: getenvDuration("TICKET_REMOTE_PHONE_RECONNECT_MAX_DELAY", 5*time.Second),
		},
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return Config{}, fmt.Errorf("TICKET_REMOTE_PORT out of range: %d", cfg.Port)
	}
	if cfg.TicketID == "" {
		return Config{}, fmt.Errorf("TICKET_REMOTE_TICKET_ID is required")
	}
	if cfg.BootstrapAdminEmail == "" {
		return Config{}, fmt.Errorf("TICKET_REMOTE_BOOTSTRAP_ADMIN_EMAIL is required")
	}
	if cfg.Phone.BaseURL == "" {
		return Config{}, fmt.Errorf("TICKET_REMOTE_PHONE_BASE_URL is required")
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func getenvInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func getenvDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	if parsed, err := time.ParseDuration(value); err == nil {
		return parsed
	}
	if hours, err := strconv.Atoi(value); err == nil {
		return time.Duration(hours) * time.Hour
	}
	return fallback
}

func splitCSV(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if clean := strings.TrimSpace(item); clean != "" {
			out = append(out, clean)
		}
	}
	return out
}

func normalizeEmail(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
