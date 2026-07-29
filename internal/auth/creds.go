// Package auth loads Kiro credentials, keeps the access token fresh and
// resolves the regions and endpoints derived from those credentials.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Flow identifies which token refresh protocol a credential set uses.
type Flow int

const (
	// FlowKiroDesktop refreshes against prod.{region}.auth.desktop.kiro.dev.
	FlowKiroDesktop Flow = iota
	// FlowAWSSSOOIDC refreshes against oidc.{region}.amazonaws.com, which
	// requires a clientId and clientSecret.
	FlowAWSSSOOIDC
)

// String renders the flow for logging.
func (f Flow) String() string {
	switch f {
	case FlowAWSSSOOIDC:
		return "aws-sso-oidc"
	default:
		return "kiro-desktop"
	}
}

// Source records where a credential set came from, so the manager knows whether
// write-back is possible.
type Source int

const (
	// SourceFile means a JSON credentials file, which kirogo may rewrite.
	SourceFile Source = iota
	// SourceEnv means REFRESH_TOKEN, which has nowhere to write back to.
	SourceEnv
	// SourceDiscovered means a JSON file found by auto-discovery. Writable.
	SourceDiscovered
	// SourceSQLite means the kiro-cli database. kirogo never writes to it.
	SourceSQLite
)

// String renders the source for logging.
func (s Source) String() string {
	switch s {
	case SourceEnv:
		return "REFRESH_TOKEN environment variable"
	case SourceDiscovered:
		return "auto-discovered credentials file"
	case SourceSQLite:
		return "kiro-cli SQLite database"
	default:
		return "credentials file"
	}
}

// Writable reports whether refreshed tokens can be persisted back to the source.
func (s Source) Writable() bool {
	return s == SourceFile || s == SourceDiscovered
}

// Credentials is a Kiro credential set, plus enough provenance to refresh it and
// write the result back without losing fields kirogo does not understand.
type Credentials struct {
	AccessToken  string
	RefreshToken string
	ProfileARN   string
	// Region is the region recorded in the credential file. It seeds both the
	// SSO region and, unless overridden, the API region.
	Region       string
	ExpiresAt    time.Time
	ClientID     string
	ClientSecret string
	// ClientIDHash points at a sibling device-registration file used by
	// enterprise Kiro IDE installs.
	ClientIDHash string
	// AuthMethod comes from the credential file, for example "social" or
	// "external_idp". It selects the TokenType request header.
	AuthMethod string

	// Source and Path record provenance.
	Source Source
	Path   string

	// extra holds every field kirogo did not recognise, so write-back preserves
	// them verbatim.
	extra map[string]json.RawMessage
}

// Flow reports which refresh protocol these credentials use. AWS SSO OIDC is
// selected only when both a client id and secret are present.
func (c *Credentials) Flow() Flow {
	if c.ClientID != "" && c.ClientSecret != "" {
		return FlowAWSSSOOIDC
	}
	return FlowKiroDesktop
}

// credsFileJSON mirrors the recognised fields of a Kiro credentials file.
type credsFileJSON struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ProfileARN   string `json:"profileArn"`
	Region       string `json:"region"`
	ExpiresAt    string `json:"expiresAt"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	ClientIDHash string `json:"clientIdHash"`
	AuthMethod   string `json:"authMethod"`
}

// knownCredsFields are the keys credsFileJSON owns. Anything else is preserved
// in Credentials.extra.
var knownCredsFields = map[string]bool{
	"accessToken": true, "refreshToken": true, "profileArn": true,
	"region": true, "expiresAt": true, "clientId": true,
	"clientSecret": true, "clientIdHash": true, "authMethod": true,
}

// LoadCredentialsFile reads and parses a JSON credentials file.
//
// When the file carries a clientIdHash (enterprise Kiro IDE), the matching
// device registration at ~/.aws/sso/cache/{clientIdHash}.json is loaded to
// supply clientId and clientSecret.
func LoadCredentialsFile(path string, source Source) (*Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read credentials file %s: %w", path, err)
	}
	creds, err := parseCredentials(data)
	if err != nil {
		return nil, fmt.Errorf("credentials file %s is not valid: %w", path, err)
	}
	creds.Source = source
	creds.Path = path

	if creds.ClientIDHash != "" && (creds.ClientID == "" || creds.ClientSecret == "") {
		if err := creds.loadDeviceRegistration(filepath.Dir(path)); err != nil {
			// Not fatal: a Kiro Desktop refresh may still work.
			return creds, nil
		}
	}
	return creds, nil
}

// parseCredentials decodes credential JSON, keeping unknown fields.
func parseCredentials(data []byte) (*Credentials, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("expected a JSON object: %w", err)
	}

	var typed credsFileJSON
	if err := json.Unmarshal(data, &typed); err != nil {
		return nil, fmt.Errorf("unexpected field types: %w", err)
	}

	creds := &Credentials{
		AccessToken:  typed.AccessToken,
		RefreshToken: typed.RefreshToken,
		ProfileARN:   typed.ProfileARN,
		Region:       typed.Region,
		ClientID:     typed.ClientID,
		ClientSecret: typed.ClientSecret,
		ClientIDHash: typed.ClientIDHash,
		AuthMethod:   typed.AuthMethod,
		extra:        map[string]json.RawMessage{},
	}
	for k, v := range raw {
		if !knownCredsFields[k] {
			creds.extra[k] = v
		}
	}

	if typed.ExpiresAt != "" {
		t, err := ParseExpiry(typed.ExpiresAt)
		if err != nil {
			// An unparseable expiry is treated as "unknown", which forces a
			// refresh rather than failing the load.
			creds.ExpiresAt = time.Time{}
		} else {
			creds.ExpiresAt = t
		}
	}

	if creds.RefreshToken == "" && creds.AccessToken == "" {
		return nil, errors.New("neither refreshToken nor accessToken is present")
	}
	return creds, nil
}

// deviceRegistrationJSON is the enterprise device registration file shape.
type deviceRegistrationJSON struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	Region       string `json:"region"`
}

// loadDeviceRegistration fills in clientId and clientSecret from the sibling
// {clientIdHash}.json file that enterprise Kiro IDE writes.
func (c *Credentials) loadDeviceRegistration(dir string) error {
	path := filepath.Join(dir, c.ClientIDHash+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("could not read device registration %s: %w", path, err)
	}
	var reg deviceRegistrationJSON
	if err := json.Unmarshal(data, &reg); err != nil {
		return fmt.Errorf("device registration %s is not valid JSON: %w", path, err)
	}
	if reg.ClientID != "" {
		c.ClientID = reg.ClientID
	}
	if reg.ClientSecret != "" {
		c.ClientSecret = reg.ClientSecret
	}
	if reg.Region != "" && c.Region == "" {
		c.Region = reg.Region
	}
	return nil
}

// fractionalSecondsPattern finds the fractional part of a timestamp so it can be
// truncated to the nanosecond precision Go's parser accepts.
var fractionalSecondsPattern = regexp.MustCompile(`\.(\d+)`)

// ParseExpiry parses an ISO-8601 / RFC3339 expiry timestamp.
//
// It tolerates a trailing Z, an explicit offset, no zone at all (treated as
// UTC), and fractional seconds of any length. kiro-cli writes nanoseconds and
// some builds write more digits than Go's parser accepts, so the fraction is
// truncated to nine digits before parsing.
func ParseExpiry(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("empty timestamp")
	}

	// Truncate an over-long fractional second to nanosecond precision.
	s = fractionalSecondsPattern.ReplaceAllStringFunc(s, func(m string) string {
		digits := m[1:]
		if len(digits) > 9 {
			digits = digits[:9]
		}
		return "." + digits
	})

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("could not parse timestamp %q as ISO-8601", s)
}

// Save writes the refreshed token material back to the credentials file at mode
// 0600, preserving every field kirogo does not manage.
//
// It is a no-op for sources that cannot be written, notably the kiro-cli SQLite
// database, which stays under kiro-cli's control.
func (c *Credentials) Save() error {
	if !c.Source.Writable() || c.Path == "" {
		return nil
	}

	out := make(map[string]json.RawMessage, len(c.extra)+9)
	for k, v := range c.extra {
		out[k] = v
	}

	set := func(key string, value any) error {
		b, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("could not encode %s: %w", key, err)
		}
		out[key] = b
		return nil
	}

	if err := set("accessToken", c.AccessToken); err != nil {
		return err
	}
	if err := set("refreshToken", c.RefreshToken); err != nil {
		return err
	}
	if !c.ExpiresAt.IsZero() {
		if err := set("expiresAt", c.ExpiresAt.UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	if c.ProfileARN != "" {
		if err := set("profileArn", c.ProfileARN); err != nil {
			return err
		}
	}
	// Preserve the remaining recognised fields we loaded but do not refresh.
	for key, value := range map[string]string{
		"region":       c.Region,
		"clientId":     c.ClientID,
		"clientSecret": c.ClientSecret,
		"clientIdHash": c.ClientIDHash,
		"authMethod":   c.AuthMethod,
	} {
		if value != "" {
			if err := set(key, value); err != nil {
				return err
			}
		}
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("could not encode credentials: %w", err)
	}
	data = append(data, '\n')

	// Write through a temporary file in the same directory so a crash cannot
	// leave a half-written credentials file behind.
	dir := filepath.Dir(c.Path)
	tmp, err := os.CreateTemp(dir, ".kirogo-creds-*")
	if err != nil {
		return fmt.Errorf("could not create a temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Remove the temporary file if it is still there after a failure.
		_ = os.Remove(tmpName)
	}()

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("could not set permissions on %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("could not write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("could not close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, c.Path); err != nil {
		return fmt.Errorf("could not replace %s: %w", c.Path, err)
	}
	return nil
}

// arnRegionPattern validates an AWS region such as us-east-1 or eu-central-1.
var arnRegionPattern = regexp.MustCompile(`^[a-z]+-[a-z]+-\d+$`)

// RegionFromARN extracts the region from a CodeWhisperer profile ARN.
//
// ARNs look like arn:aws:codewhisperer:us-east-1:123456789012:profile/ABCDEF,
// so the region is component index 3. An empty string is returned when the
// component is missing or does not look like a region.
func RegionFromARN(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) < 4 {
		return ""
	}
	if !arnRegionPattern.MatchString(parts[3]) {
		return ""
	}
	return parts[3]
}

// tokenTypeHeaders maps a credential authMethod to the TokenType request header
// the Kiro backend expects. Verified against the Kiro IDE bundle; auth methods
// absent from this map send no TokenType header at all.
var tokenTypeHeaders = map[string]string{
	"external_idp":  "EXTERNAL_IDP",
	"machine_token": "KIRO_MACHINE_TOKEN",
	"api_key":       "API_KEY",
	"IdC":           "SSO_OIDC",
}

// TokenTypeHeader returns the TokenType header value for these credentials, or
// an empty string when none applies.
func (c *Credentials) TokenTypeHeader() string {
	return tokenTypeHeaders[c.AuthMethod]
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
