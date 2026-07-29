// Package config loads and validates kirogo configuration from CLI flags,
// process environment variables and an optional .env file.
//
// Precedence is CLI flags > process environment > .env file > built-in defaults.
// The .env file is read raw (no escape-sequence processing) so that Windows
// paths such as D:\Projects\creds.json survive intact.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Version is the kirogo release version reported by /health and -version.
const Version = "1.0.0"

// DefaultProxyAPIKey is used when PROXY_API_KEY is not configured. Running with
// this value logs a loud warning because it grants access to a live AWS token.
const DefaultProxyAPIKey = "kirogo"

// ErrVersionRequested is returned by Load when -version was passed. Callers
// should print Version and exit successfully.
var ErrVersionRequested = errors.New("version requested")

// ErrHelpRequested is returned by Load when -h/-help was passed.
var ErrHelpRequested = errors.New("help requested")

// EffortLevels lists the reasoning effort levels accepted by the Kiro backend,
// verified against the Kiro IDE bundle (VALID_EFFORT_LEVELS).
var EffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// DefaultEffortLevel matches the IDE bundle's currentEffortLevel default.
const DefaultEffortLevel = "xhigh"

// retiredExact lists environment variables that other Kiro proxies define and
// kirogo deliberately does not implement. They are accepted and ignored so that
// an existing .env file still boots, with one INFO line each explaining that the
// setting has no effect here.
var retiredExact = []string{
	"SQLITE_READONLY",
	"TRUNCATION_RECOVERY",
	"AUTO_TRIM_PAYLOAD",
	"WEB_SEARCH_ENABLED",
	"DEBUG_MODE",
	"DEBUG_DIR",
	"VPN_PROXY_URL",
	"MODEL_CACHE_TTL",
	"STATE_SAVE_INTERVAL_SECONDS",
}

// retiredPrefixes lists retired variable families (matched by prefix).
var retiredPrefixes = []string{
	"FAKE_REASONING",
	"ACCOUNT_",
	"ACCOUNTS_",
}

// Config holds the fully resolved runtime configuration.
type Config struct {
	// Server
	Host string
	Port int

	// Client authentication
	ProxyAPIKey string

	// Credential sources
	CredsFile    string
	RefreshToken string
	CLIDBFile    string
	ProfileARN   string

	// Regions. SSORegion drives the token refresh endpoint; APIRegion (when set)
	// overrides the API region detected from credentials.
	SSORegion string
	APIRegion string

	// Model behaviour
	EffortLevel          string
	ExposeEffortVariants bool
	AgentMode            string
	KiroVersion          string
	ModelRefreshTTL      time.Duration

	// Streaming behaviour
	FirstTokenTimeout    time.Duration
	FirstTokenMaxRetries int
	StreamingReadTimeout time.Duration

	// Request limits
	ToolDescriptionMaxLength int
	MaxPayloadBytes          int

	// SystemPromptAsField sends the system prompt in the top-level systemPrompt
	// request field rather than folding it into the first user turn.
	//
	// The service schema declares that field, but the deployed backend answers
	// 400 REQUEST_BODY_INVALID for any request carrying it, so this defaults off.
	SystemPromptAsField bool

	// Diagnostics
	LogLevel slog.Level

	// CLI-only
	DumpModels bool

	// Warnings collected during load, emitted by the caller after the logger exists.
	Warnings []string
	// Notices are INFO-level messages (retired variables, etc.).
	Notices []string
}

// envLookup resolves a variable from the process environment first and the
// .env file second, mirroring python-dotenv's non-overriding behaviour.
type envLookup struct {
	dotenv map[string]string
}

func (e envLookup) get(key string) (string, bool) {
	if v, ok := os.LookupEnv(key); ok {
		return v, true
	}
	if v, ok := e.dotenv[key]; ok {
		return v, true
	}
	return "", false
}

// keys returns every variable name visible through this lookup, sorted.
func (e envLookup) keys() []string {
	seen := make(map[string]struct{}, len(e.dotenv))
	for k := range e.dotenv {
		seen[k] = struct{}{}
	}
	for _, kv := range os.Environ() {
		if i := strings.Index(kv, "="); i > 0 {
			seen[kv[:i]] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ParseDotEnv parses .env content without interpreting escape sequences.
//
// Supported line shapes: KEY=value, KEY="value", KEY='value', with an optional
// "export " prefix. Blank lines and lines starting with # are skipped. Values
// are stored verbatim, so a Windows path keeps its backslashes.
func ParseDotEnv(content string) map[string]string {
	out := make(map[string]string)
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if !envNamePattern.MatchString(key) {
			continue
		}
		val := strings.TrimSpace(line[eq+1:])
		if len(val) >= 2 {
			first, last := val[0], val[len(val)-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		out[key] = val
	}
	return out
}

// loadDotEnvFile reads .env from dir, returning an empty map when absent.
func loadDotEnvFile(dir string) map[string]string {
	data, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		return map[string]string{}
	}
	return ParseDotEnv(string(data))
}

// ExpandPath expands a leading ~ to the user's home directory. Paths that do
// not start with ~ are returned unchanged so Windows-style paths survive.
func ExpandPath(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		if p == "~" {
			return home
		}
		return filepath.Join(home, p[2:])
	}
	return p
}

// Load resolves configuration from args (without the program name), the process
// environment and ./.env.
func Load(args []string) (*Config, error) {
	return LoadFromDir(args, ".")
}

// LoadFromDir behaves like Load but reads .env from the given directory. It
// exists so tests can supply a fixture directory.
func LoadFromDir(args []string, dir string) (*Config, error) {
	env := envLookup{dotenv: loadDotEnvFile(dir)}

	fs := flag.NewFlagSet("kirogo", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		flagHost       = fs.String("host", "", "listen address (default 127.0.0.1 or SERVER_HOST)")
		flagPort       = fs.Int("port", 0, "listen port (default 8000 or SERVER_PORT)")
		flagVersion    = fs.Bool("version", false, "print version and exit")
		flagDumpModels = fs.Bool("dump-models", false, "print the live model catalog and exit")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, ErrHelpRequested
		}
		return nil, fmt.Errorf("invalid command line: %w", err)
	}
	if *flagVersion {
		return nil, ErrVersionRequested
	}

	provided := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { provided[f.Name] = true })

	cfg := &Config{
		Host:                     "127.0.0.1",
		Port:                     8000,
		ProxyAPIKey:              DefaultProxyAPIKey,
		SSORegion:                "us-east-1",
		ExposeEffortVariants:     true,
		AgentMode:                "vibe",
		KiroVersion:              "0.7.45",
		ModelRefreshTTL:          3600 * time.Second,
		FirstTokenTimeout:        15 * time.Second,
		FirstTokenMaxRetries:     3,
		StreamingReadTimeout:     300 * time.Second,
		ToolDescriptionMaxLength: 10000,
		MaxPayloadBytes:          600000,
		LogLevel:                 slog.LevelInfo,
		DumpModels:               *flagDumpModels,
	}

	var errs []string
	fail := func(format string, a ...any) {
		errs = append(errs, fmt.Sprintf(format, a...))
	}

	// --- strings ---
	if v, ok := env.get("SERVER_HOST"); ok && v != "" {
		cfg.Host = v
	}
	if provided["host"] {
		cfg.Host = *flagHost
	}
	if cfg.Host == "" {
		fail("SERVER_HOST is empty. Use 127.0.0.1 for local-only access or 0.0.0.0 to listen on every interface.")
	}

	if v, ok := env.get("PROXY_API_KEY"); ok && v != "" {
		cfg.ProxyAPIKey = v
	}
	if v, ok := env.get("REFRESH_TOKEN"); ok {
		cfg.RefreshToken = strings.TrimSpace(v)
	}
	if v, ok := env.get("KIRO_CREDS_FILE"); ok && v != "" {
		cfg.CredsFile = ExpandPath(v)
	}
	if v, ok := env.get("KIRO_CLI_DB_FILE"); ok && v != "" {
		cfg.CLIDBFile = ExpandPath(v)
	}
	if v, ok := env.get("PROFILE_ARN"); ok {
		cfg.ProfileARN = strings.TrimSpace(v)
	}
	if v, ok := env.get("KIRO_REGION"); ok && v != "" {
		cfg.SSORegion = v
	}
	if v, ok := env.get("KIRO_API_REGION"); ok && v != "" {
		cfg.APIRegion = v
	}
	if v, ok := env.get("KIRO_AGENT_MODE"); ok && v != "" {
		cfg.AgentMode = v
	}
	if v, ok := env.get("KIRO_VERSION"); ok && v != "" {
		cfg.KiroVersion = v
	}

	// --- effort level ---
	if v, ok := env.get("KIRO_EFFORT_LEVEL"); ok && strings.TrimSpace(v) != "" {
		level := strings.ToLower(strings.TrimSpace(v))
		if !IsValidEffortLevel(level) {
			fail("KIRO_EFFORT_LEVEL=%q is not a recognised effort level. Valid values: %s.",
				v, strings.Join(EffortLevels, ", "))
		} else {
			cfg.EffortLevel = level
		}
	}

	// --- port ---
	if v, ok := env.get("SERVER_PORT"); ok && v != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			fail("SERVER_PORT=%q is not a number. Use a port between 1 and 65535, for example SERVER_PORT=8000.", v)
		} else {
			cfg.Port = n
		}
	}
	if provided["port"] {
		cfg.Port = *flagPort
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		fail("Port %d is out of range. Use a port between 1 and 65535.", cfg.Port)
	}

	// --- booleans ---
	if v, ok := env.get("KIRO_EXPOSE_EFFORT_VARIANTS"); ok && v != "" {
		b, err := parseBool(v)
		if err != nil {
			fail("KIRO_EXPOSE_EFFORT_VARIANTS=%q is not a boolean. Use true or false.", v)
		} else {
			cfg.ExposeEffortVariants = b
		}
	}

	// --- system prompt placement ---
	if v, ok := env.get("KIRO_SYSTEM_PROMPT_MODE"); ok && strings.TrimSpace(v) != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "inline":
			cfg.SystemPromptAsField = false
		case "field":
			cfg.SystemPromptAsField = true
			cfg.Warnings = append(cfg.Warnings,
				"KIRO_SYSTEM_PROMPT_MODE=field sends the system prompt in the top-level systemPrompt field. "+
					"The Kiro backend currently rejects that field as REQUEST_BODY_INVALID, which fails every "+
					"request carrying a system prompt. Use inline unless you have confirmed the backend accepts it.")
		default:
			fail("KIRO_SYSTEM_PROMPT_MODE=%q is not recognised. Use inline, which folds the prompt into the first "+
				"message and is what the Kiro backend accepts today, or field, which sends the top-level "+
				"systemPrompt field.", v)
		}
	}

	// --- durations (seconds) ---
	cfg.ModelRefreshTTL = envSeconds(env, "KIRO_MODEL_REFRESH_TTL", cfg.ModelRefreshTTL, 1, fail)
	cfg.FirstTokenTimeout = envSeconds(env, "FIRST_TOKEN_TIMEOUT", cfg.FirstTokenTimeout, 1, fail)
	cfg.StreamingReadTimeout = envSeconds(env, "STREAMING_READ_TIMEOUT", cfg.StreamingReadTimeout, 1, fail)

	// --- integers ---
	cfg.FirstTokenMaxRetries = envInt(env, "FIRST_TOKEN_MAX_RETRIES", cfg.FirstTokenMaxRetries, 1, fail)
	cfg.ToolDescriptionMaxLength = envInt(env, "TOOL_DESCRIPTION_MAX_LENGTH", cfg.ToolDescriptionMaxLength, 0, fail)
	cfg.MaxPayloadBytes = envInt(env, "KIRO_MAX_PAYLOAD_BYTES", cfg.MaxPayloadBytes, 1024, fail)

	// --- log level ---
	if v, ok := env.get("LOG_LEVEL"); ok && v != "" {
		lvl, err := ParseLogLevel(v)
		if err != nil {
			fail("LOG_LEVEL=%q is not recognised. Valid values: DEBUG, INFO, WARN, ERROR.", v)
		} else {
			cfg.LogLevel = lvl
		}
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("configuration is invalid:\n  - %s", strings.Join(errs, "\n  - "))
	}

	// --- warnings ---
	if cfg.ProxyAPIKey == DefaultProxyAPIKey {
		cfg.Warnings = append(cfg.Warnings,
			"PROXY_API_KEY is still the default value. Anyone who can reach this port can spend your Kiro quota. Set PROXY_API_KEY to a long random string.")
	}
	if cfg.Host == "0.0.0.0" || cfg.Host == "::" {
		cfg.Warnings = append(cfg.Warnings,
			"Listening on "+cfg.Host+" exposes kirogo, and the live AWS token it holds, to your whole network. Make sure PROXY_API_KEY is a strong secret, or bind to 127.0.0.1 instead.")
	}
	if cfg.StreamingReadTimeout <= cfg.FirstTokenTimeout {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
			"STREAMING_READ_TIMEOUT (%s) should be larger than FIRST_TOKEN_TIMEOUT (%s): the first is the gap allowed between chunks, the second is how long to wait for the model to start.",
			cfg.StreamingReadTimeout, cfg.FirstTokenTimeout))
	}

	// --- retired variables ---
	for _, key := range env.keys() {
		if isRetired(key) {
			cfg.Notices = append(cfg.Notices,
				"Ignoring "+key+": that feature is not part of kirogo. The variable is accepted so existing .env files keep working.")
		}
	}

	return cfg, nil
}

// isRetired reports whether key belongs to a retired feature.
func isRetired(key string) bool {
	for _, k := range retiredExact {
		if key == k {
			return true
		}
	}
	for _, p := range retiredPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// IsValidEffortLevel reports whether level is in the backend's allowlist.
func IsValidEffortLevel(level string) bool {
	for _, l := range EffortLevels {
		if l == level {
			return true
		}
	}
	return false
}

// ParseLogLevel converts a textual log level to slog.Level.
func ParseLogLevel(s string) (slog.Level, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG", "TRACE":
		return slog.LevelDebug, nil
	case "INFO":
		return slog.LevelInfo, nil
	case "WARN", "WARNING":
		return slog.LevelWarn, nil
	case "ERROR", "CRITICAL", "FATAL":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unknown log level %q", s)
	}
}

// parseBool accepts the spellings commonly found in .env files.
func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on", "enabled":
		return true, nil
	case "0", "false", "no", "off", "disabled":
		return false, nil
	default:
		return false, fmt.Errorf("not a boolean: %q", s)
	}
}

// envSeconds reads an integer-seconds variable, enforcing a minimum.
func envSeconds(env envLookup, key string, def time.Duration, min int, fail func(string, ...any)) time.Duration {
	v, ok := env.get(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		fail("%s=%q is not a number of seconds. Example: %s=%d.", key, v, key, int(def.Seconds()))
		return def
	}
	if n < min {
		fail("%s=%d is too small. The minimum is %d seconds.", key, n, min)
		return def
	}
	return time.Duration(n) * time.Second
}

// envInt reads an integer variable, enforcing a minimum.
func envInt(env envLookup, key string, def, min int, fail func(string, ...any)) int {
	v, ok := env.get(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		fail("%s=%q is not a number. Example: %s=%d.", key, v, key, def)
		return def
	}
	if n < min {
		fail("%s=%d is too small. The minimum is %d.", key, n, min)
		return def
	}
	return n
}

// Addr returns the host:port listen address.
func (c *Config) Addr() string {
	return c.Host + ":" + strconv.Itoa(c.Port)
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
