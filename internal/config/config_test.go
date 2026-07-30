package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// allKnownVars lists every variable Load consults, plus the retired families,
// so tests can start from a clean environment regardless of the developer's
// shell. Without this, a stray PROXY_API_KEY would silently change results.
var allKnownVars = []string{
	"SERVER_HOST", "SERVER_PORT", "PROXY_API_KEY",
	"KIRO_CREDS_FILE", "REFRESH_TOKEN", "KIRO_CLI_DB_FILE", "PROFILE_ARN",
	"KIRO_REGION", "KIRO_API_REGION", "KIRO_EFFORT_LEVEL",
	"KIRO_EXPOSE_EFFORT_VARIANTS", "KIRO_AGENT_MODE", "KIRO_VERSION",
	"KIRO_SYSTEM_PROMPT_MODE",
	"KIRO_MODEL_REFRESH_TTL", "FIRST_TOKEN_TIMEOUT", "FIRST_TOKEN_MAX_RETRIES",
	"STREAMING_READ_TIMEOUT", "TOOL_DESCRIPTION_MAX_LENGTH",
	"KIRO_MAX_PAYLOAD_BYTES", "LOG_LEVEL",
	"SQLITE_READONLY", "TRUNCATION_RECOVERY", "AUTO_TRIM_PAYLOAD",
	"WEB_SEARCH_ENABLED", "DEBUG_MODE", "DEBUG_DIR", "VPN_PROXY_URL",
	"MODEL_CACHE_TTL", "STATE_SAVE_INTERVAL_SECONDS",
	"FAKE_REASONING", "FAKE_REASONING_MAX_TOKENS",
	"ACCOUNT_SYSTEM", "ACCOUNTS_CONFIG_FILE",
}

// isolateEnv clears every variable Load reads for the duration of the test.
func isolateEnv(t *testing.T) {
	t.Helper()
	for _, k := range allKnownVars {
		if v, ok := os.LookupEnv(k); ok {
			saved := v
			key := k
			if err := os.Unsetenv(key); err != nil {
				t.Fatalf("unsetenv %s: %v", key, err)
			}
			t.Cleanup(func() { _ = os.Setenv(key, saved) })
		}
	}
}

// writeDotEnv creates a .env file in a fresh temp dir and returns the dir.
func writeDotEnv(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	return dir
}

func TestDefaultsWhenNothingIsSet(t *testing.T) {
	isolateEnv(t)
	cfg, err := LoadFromDir(nil, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"Host", cfg.Host, "127.0.0.1"},
		{"Port", cfg.Port, 8000},
		{"ProxyAPIKey", cfg.ProxyAPIKey, DefaultProxyAPIKey},
		{"SSORegion", cfg.SSORegion, "us-east-1"},
		{"APIRegion", cfg.APIRegion, ""},
		{"EffortLevel", cfg.EffortLevel, ""},
		{"ExposeEffortVariants", cfg.ExposeEffortVariants, true},
		{"AgentMode", cfg.AgentMode, "vibe"},
		{"KiroVersion", cfg.KiroVersion, "0.7.45"},
		{"ModelRefreshTTL", cfg.ModelRefreshTTL, 3600 * time.Second},
		{"FirstTokenTimeout", cfg.FirstTokenTimeout, 15 * time.Second},
		{"FirstTokenMaxRetries", cfg.FirstTokenMaxRetries, 3},
		{"StreamingReadTimeout", cfg.StreamingReadTimeout, 300 * time.Second},
		{"ToolDescriptionMaxLength", cfg.ToolDescriptionMaxLength, 10000},
		{"MaxPayloadBytes", cfg.MaxPayloadBytes, 4000000},
		{"LogLevel", cfg.LogLevel, slog.LevelInfo},
		{"DumpModels", cfg.DumpModels, false},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestPrecedenceFlagBeatsEnvBeatsDotEnvBeatsDefault(t *testing.T) {
	t.Run("dotenv beats default", func(t *testing.T) {
		isolateEnv(t)
		dir := writeDotEnv(t, "SERVER_PORT=1111\nSERVER_HOST=10.0.0.1\n")
		cfg, err := LoadFromDir(nil, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Port != 1111 || cfg.Host != "10.0.0.1" {
			t.Fatalf("got %s:%d, want 10.0.0.1:1111", cfg.Host, cfg.Port)
		}
	})

	t.Run("process env beats dotenv", func(t *testing.T) {
		isolateEnv(t)
		dir := writeDotEnv(t, "SERVER_PORT=1111\n")
		t.Setenv("SERVER_PORT", "2222")
		cfg, err := LoadFromDir(nil, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Port != 2222 {
			t.Fatalf("Port = %d, want 2222", cfg.Port)
		}
	})

	t.Run("flag beats process env", func(t *testing.T) {
		isolateEnv(t)
		dir := writeDotEnv(t, "SERVER_PORT=1111\nSERVER_HOST=10.0.0.1\n")
		t.Setenv("SERVER_PORT", "2222")
		cfg, err := LoadFromDir([]string{"-port", "3333", "-host", "0.0.0.0"}, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Port != 3333 {
			t.Fatalf("Port = %d, want 3333", cfg.Port)
		}
		if cfg.Host != "0.0.0.0" {
			t.Fatalf("Host = %q, want 0.0.0.0", cfg.Host)
		}
	})
}

func TestVersionAndHelpFlags(t *testing.T) {
	isolateEnv(t)
	if _, err := LoadFromDir([]string{"-version"}, t.TempDir()); !errors.Is(err, ErrVersionRequested) {
		t.Errorf("-version error = %v, want ErrVersionRequested", err)
	}
	if _, err := LoadFromDir([]string{"-h"}, t.TempDir()); !errors.Is(err, ErrHelpRequested) {
		t.Errorf("-h error = %v, want ErrHelpRequested", err)
	}
	if _, err := LoadFromDir([]string{"-nonsense"}, t.TempDir()); err == nil {
		t.Error("unknown flag should be an error")
	}
}

func TestDumpModelsFlag(t *testing.T) {
	isolateEnv(t)
	cfg, err := LoadFromDir([]string{"-dump-models"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DumpModels {
		t.Error("DumpModels should be true")
	}
}

func TestInvalidValuesProduceActionableErrors(t *testing.T) {
	cases := []struct {
		name     string
		dotenv   string
		args     []string
		mustHave []string
	}{
		{
			name:     "unknown effort level",
			dotenv:   "KIRO_EFFORT_LEVEL=turbo\n",
			mustHave: []string{"KIRO_EFFORT_LEVEL", "turbo", "low, medium, high, xhigh, max"},
		},
		{
			name:     "non numeric port",
			dotenv:   "SERVER_PORT=abc\n",
			mustHave: []string{"SERVER_PORT", "not a number"},
		},
		{
			name:     "port out of range low",
			dotenv:   "SERVER_PORT=0\n",
			mustHave: []string{"out of range"},
		},
		{
			name:     "port out of range high",
			args:     []string{"-port", "70000"},
			mustHave: []string{"out of range"},
		},
		{
			name:     "unknown log level",
			dotenv:   "LOG_LEVEL=chatty\n",
			mustHave: []string{"LOG_LEVEL", "DEBUG, INFO, WARN, ERROR"},
		},
		{
			name:     "non boolean variant flag",
			dotenv:   "KIRO_EXPOSE_EFFORT_VARIANTS=maybe\n",
			mustHave: []string{"KIRO_EXPOSE_EFFORT_VARIANTS", "true or false"},
		},
		{
			name:     "non numeric timeout",
			dotenv:   "FIRST_TOKEN_TIMEOUT=soon\n",
			mustHave: []string{"FIRST_TOKEN_TIMEOUT", "seconds"},
		},
		{
			name:     "timeout below minimum",
			dotenv:   "FIRST_TOKEN_TIMEOUT=0\n",
			mustHave: []string{"FIRST_TOKEN_TIMEOUT", "too small"},
		},
		{
			name:     "payload limit below minimum",
			dotenv:   "KIRO_MAX_PAYLOAD_BYTES=10\n",
			mustHave: []string{"KIRO_MAX_PAYLOAD_BYTES", "too small"},
		},
		{
			name:     "retries below minimum",
			dotenv:   "FIRST_TOKEN_MAX_RETRIES=0\n",
			mustHave: []string{"FIRST_TOKEN_MAX_RETRIES", "too small"},
		},
		{
			name:     "empty host",
			dotenv:   "",
			args:     []string{"-host", ""},
			mustHave: []string{"SERVER_HOST is empty", "127.0.0.1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			dir := writeDotEnv(t, tc.dotenv)
			_, err := LoadFromDir(tc.args, dir)
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, want := range tc.mustHave {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err.Error(), want)
				}
			}
		})
	}
}

func TestMultipleInvalidValuesAreAllReported(t *testing.T) {
	isolateEnv(t)
	dir := writeDotEnv(t, "SERVER_PORT=abc\nLOG_LEVEL=chatty\nKIRO_EFFORT_LEVEL=turbo\n")
	_, err := LoadFromDir(nil, dir)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"SERVER_PORT", "LOG_LEVEL", "KIRO_EFFORT_LEVEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s, got: %s", want, err.Error())
		}
	}
}

func TestValidEffortLevelsAreAccepted(t *testing.T) {
	for _, level := range EffortLevels {
		t.Run(level, func(t *testing.T) {
			isolateEnv(t)
			dir := writeDotEnv(t, "KIRO_EFFORT_LEVEL="+strings.ToUpper(level)+"\n")
			cfg, err := LoadFromDir(nil, dir)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.EffortLevel != level {
				t.Errorf("EffortLevel = %q, want %q (lowercased)", cfg.EffortLevel, level)
			}
		})
	}
}

func TestWarnings(t *testing.T) {
	t.Run("default api key warns", func(t *testing.T) {
		isolateEnv(t)
		cfg, err := LoadFromDir(nil, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if !containsSubstring(cfg.Warnings, "PROXY_API_KEY is still the default") {
			t.Errorf("expected default-key warning, got %v", cfg.Warnings)
		}
	})

	t.Run("custom api key does not warn", func(t *testing.T) {
		isolateEnv(t)
		dir := writeDotEnv(t, "PROXY_API_KEY=a-long-random-secret\n")
		cfg, err := LoadFromDir(nil, dir)
		if err != nil {
			t.Fatal(err)
		}
		if containsSubstring(cfg.Warnings, "PROXY_API_KEY is still the default") {
			t.Errorf("unexpected default-key warning: %v", cfg.Warnings)
		}
	})

	t.Run("wildcard host warns", func(t *testing.T) {
		isolateEnv(t)
		cfg, err := LoadFromDir([]string{"-host", "0.0.0.0"}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if !containsSubstring(cfg.Warnings, "exposes kirogo") {
			t.Errorf("expected exposure warning, got %v", cfg.Warnings)
		}
	})

	t.Run("loopback host does not warn about exposure", func(t *testing.T) {
		isolateEnv(t)
		cfg, err := LoadFromDir(nil, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if containsSubstring(cfg.Warnings, "exposes kirogo") {
			t.Errorf("unexpected exposure warning: %v", cfg.Warnings)
		}
	})

	t.Run("first token timeout not below streaming read timeout warns", func(t *testing.T) {
		isolateEnv(t)
		dir := writeDotEnv(t, "FIRST_TOKEN_TIMEOUT=300\nSTREAMING_READ_TIMEOUT=300\n")
		cfg, err := LoadFromDir(nil, dir)
		if err != nil {
			t.Fatal(err)
		}
		if !containsSubstring(cfg.Warnings, "STREAMING_READ_TIMEOUT") {
			t.Errorf("expected timeout ordering warning, got %v", cfg.Warnings)
		}
	})

	t.Run("sane timeouts do not warn", func(t *testing.T) {
		isolateEnv(t)
		dir := writeDotEnv(t, "FIRST_TOKEN_TIMEOUT=15\nSTREAMING_READ_TIMEOUT=300\n")
		cfg, err := LoadFromDir(nil, dir)
		if err != nil {
			t.Fatal(err)
		}
		if containsSubstring(cfg.Warnings, "STREAMING_READ_TIMEOUT") {
			t.Errorf("unexpected timeout warning: %v", cfg.Warnings)
		}
	})
}

func TestRetiredVariablesAreAcceptedAndAnnounced(t *testing.T) {
	retired := []string{
		"FAKE_REASONING", "FAKE_REASONING_MAX_TOKENS",
		"ACCOUNT_SYSTEM", "ACCOUNTS_CONFIG_FILE",
		"SQLITE_READONLY", "TRUNCATION_RECOVERY", "AUTO_TRIM_PAYLOAD",
		"WEB_SEARCH_ENABLED", "DEBUG_MODE", "DEBUG_DIR", "VPN_PROXY_URL",
		"MODEL_CACHE_TTL", "STATE_SAVE_INTERVAL_SECONDS",
	}
	for _, name := range retired {
		t.Run(name, func(t *testing.T) {
			isolateEnv(t)
			dir := writeDotEnv(t, name+"=something\n")
			cfg, err := LoadFromDir(nil, dir)
			if err != nil {
				t.Fatalf("retired variable %s must not fail startup: %v", name, err)
			}
			if !containsSubstring(cfg.Notices, name) {
				t.Errorf("expected a notice mentioning %s, got %v", name, cfg.Notices)
			}
		})
	}
}

func TestLiveVariablesAreNotTreatedAsRetired(t *testing.T) {
	isolateEnv(t)
	dir := writeDotEnv(t, "KIRO_MODEL_REFRESH_TTL=60\nPROXY_API_KEY=secret\n")
	cfg, err := LoadFromDir(nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range cfg.Notices {
		if strings.Contains(n, "KIRO_MODEL_REFRESH_TTL") || strings.Contains(n, "PROXY_API_KEY") {
			t.Errorf("live variable reported as retired: %s", n)
		}
	}
	if cfg.ModelRefreshTTL != 60*time.Second {
		t.Errorf("ModelRefreshTTL = %v, want 60s", cfg.ModelRefreshTTL)
	}
}

func TestIsRetired(t *testing.T) {
	cases := map[string]bool{
		"FAKE_REASONING":              true,
		"FAKE_REASONING_HANDLING":     true,
		"ACCOUNT_RECOVERY_TIMEOUT":    true,
		"ACCOUNTS_STATE_FILE":         true,
		"SQLITE_READONLY":             true,
		"MODEL_CACHE_TTL":             true,
		"KIRO_MODEL_REFRESH_TTL":      false,
		"PROXY_API_KEY":               false,
		"KIRO_EXPOSE_EFFORT_VARIANTS": false,
		"PATH":                        false,
	}
	for key, want := range cases {
		if got := isRetired(key); got != want {
			t.Errorf("isRetired(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestParseDotEnv(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    map[string]string
	}{
		{
			name:    "windows path keeps backslashes",
			content: `KIRO_CREDS_FILE=D:\Projects\adolf\creds.json`,
			want:    map[string]string{"KIRO_CREDS_FILE": `D:\Projects\adolf\creds.json`},
		},
		{
			name:    "double quoted windows path",
			content: `KIRO_CREDS_FILE="D:\a\nb\tc"`,
			want:    map[string]string{"KIRO_CREDS_FILE": `D:\a\nb\tc`},
		},
		{
			name:    "single quoted",
			content: `PROXY_API_KEY='my secret'`,
			want:    map[string]string{"PROXY_API_KEY": "my secret"},
		},
		{
			name:    "comments and blanks skipped",
			content: "# a comment\n\n   \nA=1\n  # indented comment\nB=2\n",
			want:    map[string]string{"A": "1", "B": "2"},
		},
		{
			name:    "export prefix",
			content: "export A=1\n",
			want:    map[string]string{"A": "1"},
		},
		{
			name:    "value containing equals",
			content: "URL=http://x/?a=b&c=d\n",
			want:    map[string]string{"URL": "http://x/?a=b&c=d"},
		},
		{
			name:    "crlf line endings",
			content: "A=1\r\nB=2\r\n",
			want:    map[string]string{"A": "1", "B": "2"},
		},
		{
			name:    "empty value",
			content: "A=\n",
			want:    map[string]string{"A": ""},
		},
		{
			name:    "no equals sign ignored",
			content: "JUST_A_WORD\n",
			want:    map[string]string{},
		},
		{
			name:    "leading equals ignored",
			content: "=value\n",
			want:    map[string]string{},
		},
		{
			name:    "invalid identifier ignored",
			content: "not-a-valid-name=1\n1BAD=2\n",
			want:    map[string]string{},
		},
		{
			name:    "unbalanced quote kept verbatim",
			content: `A="unclosed`,
			want:    map[string]string{"A": `"unclosed`},
		},
		{
			name:    "single character value",
			content: "A=x\n",
			want:    map[string]string{"A": "x"},
		},
		{
			name:    "quoted empty string",
			content: `A=""`,
			want:    map[string]string{"A": ""},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseDotEnv(tc.content)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d entries %v, want %d entries %v", len(got), got, len(tc.want), tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("key %q = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestDotEnvPathIsExpandedAndPreserved(t *testing.T) {
	isolateEnv(t)
	dir := writeDotEnv(t, `KIRO_CREDS_FILE=D:\creds\token.json`+"\n")
	cfg, err := LoadFromDir(nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CredsFile != `D:\creds\token.json` {
		t.Errorf("CredsFile = %q, want the Windows path unchanged", cfg.CredsFile)
	}
}

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory available")
	}
	cases := []struct{ in, want string }{
		{"", ""},
		{"~", home},
		{"~/x/y.json", filepath.Join(home, "x/y.json")},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
		{`D:\windows\path`, `D:\windows\path`},
		{"~notahome/x", "~notahome/x"},
	}
	for _, c := range cases {
		if got := ExpandPath(c.in); got != c.want {
			t.Errorf("ExpandPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseLogLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"TRACE":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"  INFO ": slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"WARNING": slog.LevelWarn,
		"error":   slog.LevelError,
		"FATAL":   slog.LevelError,
	}
	for in, want := range cases {
		got, err := ParseLogLevel(in)
		if err != nil {
			t.Errorf("ParseLogLevel(%q) returned error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseLogLevel(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseLogLevel("verbose"); err == nil {
		t.Error("ParseLogLevel(\"verbose\") should fail")
	}
}

func TestParseBool(t *testing.T) {
	truthy := []string{"1", "true", "TRUE", "yes", "on", "enabled", " true "}
	falsy := []string{"0", "false", "FALSE", "no", "off", "disabled"}
	for _, s := range truthy {
		v, err := parseBool(s)
		if err != nil || !v {
			t.Errorf("parseBool(%q) = %v, %v; want true, nil", s, v, err)
		}
	}
	for _, s := range falsy {
		v, err := parseBool(s)
		if err != nil || v {
			t.Errorf("parseBool(%q) = %v, %v; want false, nil", s, v, err)
		}
	}
	if _, err := parseBool("perhaps"); err == nil {
		t.Error("parseBool(\"perhaps\") should fail")
	}
}

func TestAddr(t *testing.T) {
	cfg := &Config{Host: "127.0.0.1", Port: 9000}
	if got := cfg.Addr(); got != "127.0.0.1:9000" {
		t.Errorf("Addr() = %q, want 127.0.0.1:9000", got)
	}
}

func TestMissingDotEnvIsNotAnError(t *testing.T) {
	isolateEnv(t)
	if _, err := LoadFromDir(nil, filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Errorf("absent .env should be fine, got %v", err)
	}
}

// containsSubstring reports whether any element of list contains sub.
func containsSubstring(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestSystemPromptMode(t *testing.T) {
	t.Run("defaults to inline", func(t *testing.T) {
		isolateEnv(t)
		cfg, err := LoadFromDir(nil, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if cfg.SystemPromptAsField {
			t.Error("the default must be inline, because the backend rejects the top-level field")
		}
	})

	t.Run("inline is accepted explicitly", func(t *testing.T) {
		isolateEnv(t)
		dir := writeDotEnv(t, "KIRO_SYSTEM_PROMPT_MODE=inline\n")
		cfg, err := LoadFromDir(nil, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.SystemPromptAsField {
			t.Error("inline should leave SystemPromptAsField false")
		}
	})

	t.Run("field is accepted and warns", func(t *testing.T) {
		isolateEnv(t)
		dir := writeDotEnv(t, "KIRO_SYSTEM_PROMPT_MODE=FIELD\n")
		cfg, err := LoadFromDir(nil, dir)
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.SystemPromptAsField {
			t.Error("field should set SystemPromptAsField true")
		}
		if !containsSubstring(cfg.Warnings, "REQUEST_BODY_INVALID") {
			t.Errorf("choosing field should warn that the backend rejects it, got %v", cfg.Warnings)
		}
	})

	t.Run("an unknown mode is an actionable error", func(t *testing.T) {
		isolateEnv(t)
		dir := writeDotEnv(t, "KIRO_SYSTEM_PROMPT_MODE=somewhere-else\n")
		_, err := LoadFromDir(nil, dir)
		if err == nil {
			t.Fatal("expected an error")
		}
		for _, want := range []string{"KIRO_SYSTEM_PROMPT_MODE", "inline", "field"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q should mention %q", err, want)
			}
		}
	})
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
