package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/user"
)

// fingerprintSalt distinguishes kirogo's fingerprint from any other client's on
// the same machine. Its value is arbitrary; only its stability matters.
const fingerprintSalt = "-kirogo"

// MachineFingerprint returns a stable per-installation identifier used inside the
// User-Agent header.
//
// The formula is sha256("{hostname}-{username}-kirogo") in lowercase hex. The
// backend treats it as an opaque client id, and it contains no secret material:
// a hostname and a login name are not credentials, and the hash is one way.
func MachineFingerprint() string {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown-host"
	}
	sum := sha256.Sum256([]byte(hostname + "-" + currentUsername() + fingerprintSalt))
	return hex.EncodeToString(sum[:])
}

// currentUsername resolves the login name the same way Python's
// getpass.getuser() does: the LOGNAME, USER, LNAME and USERNAME environment
// variables in that order, then the password database.
func currentUsername() string {
	for _, key := range []string{"LOGNAME", "USER", "LNAME", "USERNAME"} {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown-user"
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
