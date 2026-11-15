// Package flagsmith is the flag service's link to one hosted Flagsmith
// environment (services.md §7.3, UC-054, UC-055, architecture note
// flagsmith-sync.md): config, the HTTP client, the in-memory link state and
// the link's run loop. Flagsmith is a writer, not an evaluator (F1): pulled
// changes are in-process writes labelled WriterLabel; local changes reach the
// link through the state.Observer. The link is off by default and never stops
// the service from starting.
package flagsmith

import (
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config defaults and bounds (services.md §7.3 Config).
const (
	DefaultAPIURL      = "https://api.flagsmith.com/api/v1/"
	DefaultDocumentURL = "https://edge.api.flagsmith.com/api/v1/"
	DefaultPoll        = 30 * time.Second
	DefaultPushRate    = 5
	DefaultTimeout     = 5 * time.Second

	MinPollSeconds    = 5
	MaxPollSeconds    = 30 // NFR-058: a Flagsmith change reaches the stack within one poll
	MinPushRate       = 1
	MaxPushRate       = 50
	MinTimeoutSeconds = 1
	MaxTimeoutSeconds = 60
)

// Config is the link's configuration. The keys and the webhook URL are
// secrets: they are never logged, put on a span, or included in an error.
type Config struct {
	// Enabled is FLAGSMITH_LINK=on with every required value present.
	Enabled bool
	// APIURL is the Admin API base (ends with "/").
	APIURL string
	// DocumentURL is the base the environment document is pulled from (ends
	// with "/"); the SaaS serves identity overrides only from the edge API.
	DocumentURL      string
	ServerKey        string
	AdminKey         string
	EnvironmentKey   string
	ProjectID        int
	Poll             time.Duration
	RealtimeURL      string
	PushRate         int
	HealthWebhookURL string
	Timeout          time.Duration
	// ConfigError names the missing or invalid variables (never their
	// values); GET /state shows it as flagsmith.config_error.
	ConfigError []string
}

// defaultConfig is the configuration with every default and the link off.
func defaultConfig() Config {
	return Config{
		APIURL:      DefaultAPIURL,
		DocumentURL: DefaultDocumentURL,
		Poll:        DefaultPoll,
		PushRate:    DefaultPushRate,
		Timeout:     DefaultTimeout,
	}
}

// LoadConfig reads the FLAGSMITH_* environment. FLAGSMITH_LINK unset or off:
// the link is off and nothing else is read. on: the link is on when
// FLAGSMITH_SERVER_KEY, FLAGSMITH_ADMIN_API_KEY, FLAGSMITH_ENVIRONMENT_KEY and
// FLAGSMITH_PROJECT_ID (a positive integer) are set. A missing or invalid
// required value, or a FLAGSMITH_LINK other than on/off, leaves the link off;
// an invalid tuning value falls back to its default. Either way one error log
// names the variables, never their values, and the service starts.
func LoadConfig(logger *slog.Logger) Config {
	if logger == nil {
		logger = slog.Default()
	}
	c := defaultConfig()
	switch link := strings.TrimSpace(os.Getenv("FLAGSMITH_LINK")); link {
	case "", "off":
		return c
	case "on":
	default:
		c.ConfigError = []string{"FLAGSMITH_LINK"}
		logger.Error("flagsmith link off: FLAGSMITH_LINK must be on or off", "invalid", c.ConfigError)
		return c
	}
	var missing, invalid []string
	// Literal os.Getenv keys: tests/contract/env_inventory.py attributes reads
	// by their key.
	required := func(value, name string) string {
		v := strings.TrimSpace(value)
		if v == "" {
			missing = append(missing, name)
		}
		return v
	}
	c.ServerKey = required(os.Getenv("FLAGSMITH_SERVER_KEY"), "FLAGSMITH_SERVER_KEY")
	c.AdminKey = required(os.Getenv("FLAGSMITH_ADMIN_API_KEY"), "FLAGSMITH_ADMIN_API_KEY")
	c.EnvironmentKey = required(os.Getenv("FLAGSMITH_ENVIRONMENT_KEY"), "FLAGSMITH_ENVIRONMENT_KEY")
	if v := required(os.Getenv("FLAGSMITH_PROJECT_ID"), "FLAGSMITH_PROJECT_ID"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			missing = append(missing, "FLAGSMITH_PROJECT_ID")
		} else {
			c.ProjectID = n
		}
	}
	if v, ok := baseURL(os.Getenv("FLAGSMITH_API_URL")); ok {
		if v != "" {
			c.APIURL = v
		}
	} else {
		invalid = append(invalid, "FLAGSMITH_API_URL")
	}
	if v, ok := baseURL(os.Getenv("FLAGSMITH_DOCUMENT_URL")); ok {
		if v != "" {
			c.DocumentURL = v
		}
	} else {
		invalid = append(invalid, "FLAGSMITH_DOCUMENT_URL")
	}
	if v, ok := plainURL(os.Getenv("FLAGSMITH_REALTIME_URL")); ok {
		c.RealtimeURL = v
	} else {
		invalid = append(invalid, "FLAGSMITH_REALTIME_URL")
	}
	if v, ok := plainURL(os.Getenv("FLAGSMITH_HEALTH_WEBHOOK_URL")); ok {
		c.HealthWebhookURL = v
	} else {
		invalid = append(invalid, "FLAGSMITH_HEALTH_WEBHOOK_URL")
	}
	if n, ok := intIn(os.Getenv("FLAGSMITH_POLL_SECONDS"), MinPollSeconds, MaxPollSeconds); ok {
		if n > 0 {
			c.Poll = time.Duration(n) * time.Second
		}
	} else {
		invalid = append(invalid, "FLAGSMITH_POLL_SECONDS")
	}
	if n, ok := intIn(os.Getenv("FLAGSMITH_PUSH_RATE"), MinPushRate, MaxPushRate); ok {
		if n > 0 {
			c.PushRate = n
		}
	} else {
		invalid = append(invalid, "FLAGSMITH_PUSH_RATE")
	}
	if n, ok := intIn(os.Getenv("FLAGSMITH_TIMEOUT_SECONDS"), MinTimeoutSeconds, MaxTimeoutSeconds); ok {
		if n > 0 {
			c.Timeout = time.Duration(n) * time.Second
		}
	} else {
		invalid = append(invalid, "FLAGSMITH_TIMEOUT_SECONDS")
	}
	c.ConfigError = append(append([]string(nil), missing...), invalid...)
	if len(missing) > 0 {
		c.ServerKey, c.AdminKey, c.EnvironmentKey = "", "", ""
		logger.Error("flagsmith link off: required variables missing or invalid", "missing", missing, "invalid", invalid)
		return c
	}
	if len(invalid) > 0 {
		logger.Error("flagsmith link: invalid variables, using their defaults", "invalid", invalid)
	}
	c.Enabled = true
	return c
}

// intIn parses an optional integer within [lo, hi]: ("", true) is 0 (unset).
func intIn(s string, lo, hi int) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < lo || n > hi {
		return 0, false
	}
	return n, true
}

// plainURL validates an optional absolute http(s) URL ("" is unset).
func plainURL(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", true
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	return s, true
}

// baseURL is plainURL normalised to end with "/".
func baseURL(s string) (string, bool) {
	v, ok := plainURL(s)
	if !ok || v == "" {
		return v, ok
	}
	if !strings.HasSuffix(v, "/") {
		v += "/"
	}
	return v, true
}
