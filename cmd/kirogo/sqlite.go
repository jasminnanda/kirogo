package main

import (
	"kirogo/internal/auth"
	"kirogo/internal/store"
)

// sqliteLoader reads credentials from a kiro-cli SQLite database.
//
// It is a package-level variable so the credential source stays optional: a build
// without it produces a clear message for KIRO_CLI_DB_FILE rather than a silent
// failure. The reader is read-only, so token rotation stays kiro-cli's job.
var sqliteLoader auth.SQLiteLoader = loadFromKiroCLI

// loadFromKiroCLI adapts the store package's result to the auth layer.
func loadFromKiroCLI(path string) (*auth.Credentials, error) {
	creds, err := store.LoadCredentials(path)
	if err != nil {
		return nil, err
	}
	return &auth.Credentials{
		AccessToken:  creds.AccessToken,
		RefreshToken: creds.RefreshToken,
		ProfileARN:   creds.ProfileARN,
		Region:       creds.Region,
		ExpiresAt:    creds.ExpiresAt,
		ClientID:     creds.ClientID,
		ClientSecret: creds.ClientSecret,
		// The kiro-cli source is never written back to, so no path is recorded.
		Source: auth.SourceSQLite,
	}, nil
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
