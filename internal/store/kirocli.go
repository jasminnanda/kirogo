package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// Table and column names used by kiro-cli.
const (
	authTable   = "auth_kv"
	stateTable  = "state"
	keyColumn   = "key"
	valueColumn = "value"
)

// TokenKeys are the auth_kv keys that may hold a token, in priority order.
//
// The "odic" spelling is a typo in kiro-cli itself. It is reproduced exactly,
// because that is the key the data is actually stored under.
var TokenKeys = []string{
	"kirocli:social:token",
	"kirocli:odic:token",
	"codewhisperer:odic:token",
}

// RegistrationKeys are the auth_kv keys that may hold a device registration.
var RegistrationKeys = []string{
	"kirocli:odic:device-registration",
	"codewhisperer:odic:device-registration",
}

// ProfileStateKey is the state row holding the selected CodeWhisperer profile.
const ProfileStateKey = "api.codewhisperer.profile"

// tokenJSON is the token record kiro-cli writes. Its field names are snake_case,
// unlike the camelCase used by Kiro IDE.
type tokenJSON struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	ProfileARN   string   `json:"profile_arn"`
	Region       string   `json:"region"`
	Scopes       []string `json:"scopes"`
	ExpiresAt    string   `json:"expires_at"`
}

// registrationJSON is the device registration record.
type registrationJSON struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Region       string `json:"region"`
}

// profileJSON is the selected profile record in the state table.
type profileJSON struct {
	ARN string `json:"arn"`
}

// Credentials is what a kiro-cli database yields.
//
// It mirrors the fields the auth layer needs, without importing it, so the two
// packages stay independent.
type Credentials struct {
	AccessToken  string
	RefreshToken string
	ProfileARN   string
	Region       string
	ExpiresAt    time.Time
	ClientID     string
	ClientSecret string
	// SourceKey records which auth_kv key the token came from, for logging.
	SourceKey string
}

// LoadCredentials reads credentials from a kiro-cli database.
//
// It never writes: token rotation stays kiro-cli's responsibility, so kirogo
// keeps a refreshed token in memory only.
func LoadCredentials(path string) (*Credentials, error) {
	db, err := Open(path)
	if err != nil {
		return nil, err
	}

	creds := &Credentials{}

	// The token, from whichever key holds one.
	var lastErr error
	for _, key := range TokenKeys {
		value, err := db.Lookup(authTable, keyColumn, valueColumn, key)
		if err != nil {
			if !errors.Is(err, ErrNotFound) {
				lastErr = err
			}
			continue
		}
		var token tokenJSON
		if err := json.Unmarshal([]byte(value.Text()), &token); err != nil {
			slog.Debug("a kiro-cli token row is not valid JSON, trying the next key",
				"key", key, "error", err.Error())
			continue
		}
		if token.AccessToken == "" && token.RefreshToken == "" {
			slog.Debug("a kiro-cli token row carries no tokens, trying the next key", "key", key)
			continue
		}

		creds.AccessToken = token.AccessToken
		creds.RefreshToken = token.RefreshToken
		creds.ProfileARN = token.ProfileARN
		creds.Region = token.Region
		creds.SourceKey = key

		if token.ExpiresAt != "" {
			if t, err := ParseTimestamp(token.ExpiresAt); err == nil {
				creds.ExpiresAt = t
			} else {
				// An unreadable expiry is treated as unknown, which forces a
				// refresh rather than failing the load.
				slog.Debug("could not parse a kiro-cli expires_at, treating the token as expiring now",
					"value", token.ExpiresAt, "error", err.Error())
			}
		}
		break
	}

	if creds.AccessToken == "" && creds.RefreshToken == "" {
		if lastErr != nil {
			return nil, fmt.Errorf("could not read a token from %s: %w", path, lastErr)
		}
		tables, _ := db.Tables()
		return nil, fmt.Errorf("no kiro-cli token found in %s. Looked for %s in the %s table; "+
			"the database contains these tables: %s. Run 'kiro-cli login' to create one",
			path, strings.Join(TokenKeys, ", "), authTable, strings.Join(tables, ", "))
	}

	// The device registration, which supplies the OIDC client credentials.
	for _, key := range RegistrationKeys {
		value, err := db.Lookup(authTable, keyColumn, valueColumn, key)
		if err != nil {
			continue
		}
		var reg registrationJSON
		if err := json.Unmarshal([]byte(value.Text()), &reg); err != nil {
			slog.Debug("a kiro-cli device registration is not valid JSON", "key", key, "error", err.Error())
			continue
		}
		if reg.ClientID != "" {
			creds.ClientID = reg.ClientID
		}
		if reg.ClientSecret != "" {
			creds.ClientSecret = reg.ClientSecret
		}
		if reg.Region != "" && creds.Region == "" {
			creds.Region = reg.Region
		}
		break
	}

	// The selected profile, when the token itself did not carry one.
	if creds.ProfileARN == "" {
		if value, err := db.Lookup(stateTable, keyColumn, valueColumn, ProfileStateKey); err == nil {
			var profile profileJSON
			if err := json.Unmarshal([]byte(value.Text()), &profile); err == nil && profile.ARN != "" {
				creds.ProfileARN = profile.ARN
			}
		}
	}

	slog.Debug("loaded credentials from a kiro-cli database",
		"source_key", creds.SourceKey,
		"has_client_credentials", creds.ClientID != "" && creds.ClientSecret != "",
		"profile_arn_present", creds.ProfileARN != "",
		"region", creds.Region)

	return creds, nil
}

// overlongFraction matches a fractional second with more digits than Go parses.
var overlongFraction = regexp.MustCompile(`\.(\d+)`)

// ParseTimestamp parses an RFC3339 timestamp from a kiro-cli record.
//
// kiro-cli writes nanosecond precision, and some builds write more digits than
// Go's parser accepts, so the fraction is truncated to nine digits first.
func ParseTimestamp(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("empty timestamp")
	}

	s = overlongFraction.ReplaceAllStringFunc(s, func(m string) string {
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
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("could not parse %q as an RFC3339 timestamp", s)
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
