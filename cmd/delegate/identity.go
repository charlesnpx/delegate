package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"unicode/utf8"
)

const workspaceKeyPrefix = "delegate-v1-"

var newRequestID = randomRequestID

func randomRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "delegate-" + hex.EncodeToString(b[:]), nil
}

func validateRequestID(requestID string) error {
	if requestID == "" || len(requestID) > 256 || !utf8.ValidString(requestID) {
		return fmt.Errorf("invalid request id %q", requestID)
	}
	for _, r := range requestID {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("invalid request id %q", requestID)
		}
	}
	return nil
}

func workspaceKeyForLogicalWorkspace(path string) (string, error) {
	canonical, err := canonicalLogicalWorkspace(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return workspaceKeyPrefix + hex.EncodeToString(sum[:]), nil
}

func canonicalLogicalWorkspace(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("logical workspace is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(abs)
	if evaluated, err := filepath.EvalSymlinks(clean); err == nil {
		return filepath.Clean(evaluated), nil
	}
	return evalSymlinksAsFeasible(clean), nil
}
