package flagsmith

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

var allVars = []string{
	"FLAGSMITH_LINK", "FLAGSMITH_API_URL", "FLAGSMITH_DOCUMENT_URL", "FLAGSMITH_SERVER_KEY", "FLAGSMITH_ADMIN_API_KEY",
	"FLAGSMITH_ENVIRONMENT_KEY", "FLAGSMITH_PROJECT_ID", "FLAGSMITH_POLL_SECONDS", "FLAGSMITH_REALTIME_URL",
	"FLAGSMITH_PUSH_RATE", "FLAGSMITH_HEALTH_WEBHOOK_URL", "FLAGSMITH_TIMEOUT_SECONDS",
}

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range allVars {
		t.Setenv(k, kv[k])
	}
}

func fullEnv() map[string]string {
	return map[string]string{
		"FLAGSMITH_LINK":            "on",
		"FLAGSMITH_SERVER_KEY":      "ser.secretserverkey",
		"FLAGSMITH_ADMIN_API_KEY":   "secretadminkey",
		"FLAGSMITH_ENVIRONMENT_KEY": "envkey123",
		"FLAGSMITH_PROJECT_ID":      "42",
	}
}

func load(t *testing.T, kv map[string]string) (Config, string) {
	t.Helper()
	setEnv(t, kv)
	var buf bytes.Buffer
	c := LoadConfig(slog.New(slog.NewTextHandler(&buf, nil)))
	return c, buf.String()
}

// UC: UC-054 A1
func TestLoadConfig(t *testing.T) {
	t.Run("UC-054/A1 unset is off, nothing logged", func(t *testing.T) {
		c, log := load(t, nil)
		if c.Enabled || log != "" || len(c.ConfigError) != 0 {
			t.Fatalf("config %+v log %q, want off and silent", c, log)
		}
	})
	t.Run("UC-054/A1 on with every required value is on with the defaults", func(t *testing.T) {
		c, log := load(t, fullEnv())
		if !c.Enabled || log != "" {
			t.Fatalf("config %+v log %q, want on", c, log)
		}
		if c.Poll != 30*time.Second || c.PushRate != 5 || c.Timeout != 5*time.Second || c.ProjectID != 42 {
			t.Fatalf("config %+v, want 30 s / 5 / 5 s / project 42", c)
		}
		if c.APIURL != DefaultAPIURL || c.DocumentURL != DefaultDocumentURL {
			t.Fatalf("urls %q %q, want the defaults", c.APIURL, c.DocumentURL)
		}
	})
	t.Run("UC-054/A1 a missing admin key keeps the link off and names the variable, never a value", func(t *testing.T) {
		env := fullEnv()
		delete(env, "FLAGSMITH_ADMIN_API_KEY")
		c, log := load(t, env)
		if c.Enabled {
			t.Fatal("link on without an admin key")
		}
		if !strings.Contains(log, "FLAGSMITH_ADMIN_API_KEY") || strings.Contains(log, "secretserverkey") || strings.Contains(log, "envkey123") {
			t.Fatalf("log %q, want the name and no key value", log)
		}
		if len(c.ConfigError) != 1 || c.ConfigError[0] != "FLAGSMITH_ADMIN_API_KEY" || c.ServerKey != "" {
			t.Fatalf("config error %v server key set %v", c.ConfigError, c.ServerKey != "")
		}
	})
	t.Run("UC-054/A1 a project id that is not a positive integer keeps the link off", func(t *testing.T) {
		env := fullEnv()
		env["FLAGSMITH_PROJECT_ID"] = "abc"
		if c, _ := load(t, env); c.Enabled {
			t.Fatal("link on with a bad project id")
		}
	})
	t.Run("UC-054/A1 FLAGSMITH_LINK=true is off with an error", func(t *testing.T) {
		env := fullEnv()
		env["FLAGSMITH_LINK"] = "true"
		c, log := load(t, env)
		if c.Enabled || !strings.Contains(log, "FLAGSMITH_LINK") || len(c.ConfigError) != 1 {
			t.Fatalf("config %+v log %q, want off with an error", c, log)
		}
	})
	t.Run("UC-054/A1 bad tuning values fall back to the defaults with an error, the link stays on", func(t *testing.T) {
		env := fullEnv()
		env["FLAGSMITH_POLL_SECONDS"] = "abc"
		env["FLAGSMITH_PUSH_RATE"] = "500"
		env["FLAGSMITH_TIMEOUT_SECONDS"] = "0"
		env["FLAGSMITH_API_URL"] = "ftp://x"
		c, log := load(t, env)
		if !c.Enabled || c.Poll != DefaultPoll || c.PushRate != DefaultPushRate || c.Timeout != DefaultTimeout || c.APIURL != DefaultAPIURL {
			t.Fatalf("config %+v, want on with the defaults", c)
		}
		for _, name := range []string{"FLAGSMITH_POLL_SECONDS", "FLAGSMITH_PUSH_RATE", "FLAGSMITH_TIMEOUT_SECONDS", "FLAGSMITH_API_URL"} {
			if !strings.Contains(log, name) {
				t.Fatalf("log %q does not name %s", log, name)
			}
		}
	})
	t.Run("UC-054/A1 poll is bounded by NFR-058 (5..30 s) and URLs end with a slash", func(t *testing.T) {
		env := fullEnv()
		env["FLAGSMITH_POLL_SECONDS"] = "31"
		env["FLAGSMITH_DOCUMENT_URL"] = "http://edge.example/api/v1"
		c, _ := load(t, env)
		if c.Poll != DefaultPoll || c.DocumentURL != "http://edge.example/api/v1/" {
			t.Fatalf("poll %v document %q", c.Poll, c.DocumentURL)
		}
		env["FLAGSMITH_POLL_SECONDS"] = "5"
		if c, _ := load(t, env); c.Poll != 5*time.Second {
			t.Fatalf("poll %v, want 5 s", c.Poll)
		}
	})
}
