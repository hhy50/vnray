// Package authkey generates and parses client transport keys.
package authkey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

// Key is a 256-bit pre-shared transport key.
type Key [32]byte

// ID identifies a key without transmitting the secret.
type ID [32]byte

// ErrInvalid indicates a missing or malformed generated key.
var ErrInvalid = errors.New("auth_key must be a generated 32-byte Base64 key")

// Generate creates a key using the operating system random source.
func Generate() string {
	var key Key
	rand.Read(key[:])
	return base64.StdEncoding.EncodeToString(key[:])
}

// Parse accepts only canonical Base64 encoding of a 256-bit key.
func Parse(value string) (Key, error) {
	var key Key
	b, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || len(b) != len(key) || base64.StdEncoding.EncodeToString(b) != value {
		return key, ErrInvalid
	}
	copy(key[:], b)
	return key, nil
}

// ID returns the public, domain-separated identifier of the key.
func (k Key) ID() ID {
	return sha256.Sum256(append([]byte("vnray transport key id v1\x00"), k[:]...))
}
