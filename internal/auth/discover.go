package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ssoCacheDir returns the AWS SSO cache directory used by Kiro IDE.
func ssoCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine your home directory: %w", err)
	}
	return filepath.Join(home, ".aws", "sso", "cache"), nil
}

// PreferredCredentialFile is the file Kiro IDE writes on sign-in.
const PreferredCredentialFile = "kiro-auth-token.json"

// Discover looks for a usable Kiro credentials file without any configuration.
//
// It prefers ~/.aws/sso/cache/kiro-auth-token.json, then falls back to any other
// JSON file in that directory that carries a refreshToken. Device registration
// files, which hold only a clientId and clientSecret, are skipped.
func Discover() (string, error) {
	dir, err := ssoCacheDir()
	if err != nil {
		return "", err
	}

	preferred := filepath.Join(dir, PreferredCredentialFile)
	if fileHasRefreshToken(preferred) {
		return preferred, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("could not list %s: %w", dir, err)
	}

	var candidates []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if e.Name() == PreferredCredentialFile {
			continue // already tried, and it had no refreshToken
		}
		path := filepath.Join(dir, e.Name())
		if fileHasRefreshToken(path) {
			candidates = append(candidates, path)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no credentials file with a refreshToken found in %s", dir)
	}

	// Deterministic choice: newest modification time first, name as tiebreaker,
	// because the most recently written cache entry is the live session.
	sort.Slice(candidates, func(i, j int) bool {
		fi, erri := os.Stat(candidates[i])
		fj, errj := os.Stat(candidates[j])
		if erri == nil && errj == nil && !fi.ModTime().Equal(fj.ModTime()) {
			return fi.ModTime().After(fj.ModTime())
		}
		return candidates[i] < candidates[j]
	})
	return candidates[0], nil
}

// fileHasRefreshToken reports whether path is JSON containing a non-empty
// refreshToken field.
func fileHasRefreshToken(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var probe struct {
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	return probe.RefreshToken != ""
}

// MissingCredentialsError explains, in full, how to give kirogo a credential.
type MissingCredentialsError struct {
	// Attempted lists the sources kirogo checked, in order.
	Attempted []string
}

// Error renders the actionable message.
func (e *MissingCredentialsError) Error() string {
	dir, err := ssoCacheDir()
	if err != nil {
		dir = "~/.aws/sso/cache"
	}
	var b strings.Builder
	b.WriteString("No Kiro credentials found. kirogo looks for them in this order:\n")
	b.WriteString("  1. KIRO_CREDS_FILE, a path to a Kiro credentials JSON file\n")
	b.WriteString("  2. REFRESH_TOKEN, a refresh token supplied directly\n")
	b.WriteString("  3. " + filepath.Join(dir, PreferredCredentialFile) + ", then any other *.json in\n")
	b.WriteString("     " + dir + " that contains a refreshToken\n")
	b.WriteString("  4. KIRO_CLI_DB_FILE, the kiro-cli SQLite database\n\n")
	b.WriteString("The usual fix is to install Kiro IDE and sign in once: that writes\n")
	b.WriteString(filepath.Join(dir, PreferredCredentialFile) + " and kirogo will find it with no configuration.\n")
	if len(e.Attempted) > 0 {
		b.WriteString("\nWhat kirogo tried:\n")
		for _, a := range e.Attempted {
			b.WriteString("  - " + a + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
