package main

import (
	"crypto/rand"
	"encoding/hex"
)

var newJobID = randomJobID

func randomJobID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "job_" + hex.EncodeToString(b[:]), nil
}
