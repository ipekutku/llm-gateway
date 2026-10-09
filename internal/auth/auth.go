// Package auth identifies gateway clients by their API keys.
//
// The gateway stores only the SHA-256 hash of each key. A presented key is
// hashed and looked up by its hash, so no comparison ever runs over the
// secret itself, and the configuration holds nothing that can be used to
// authenticate.
//
// Unsalted SHA-256 is appropriate only because gateway keys are random,
// high-entropy secrets, not passwords: a hash cannot be reversed by
// guessing. Keys must be generated accordingly, for example with
// `openssl rand -base64 32`.
package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
)

// maxClientIDLength bounds client IDs, which appear in logs.
const maxClientIDLength = 64

var (
	// ErrInvalidKey reports a key that belongs to no client.
	ErrInvalidKey = errors.New("invalid API key")
	// ErrDisabledKey reports a key that belongs to a disabled client.
	ErrDisabledKey = errors.New("API key disabled")
)

// Client is a gateway client and its API key.
type Client struct {
	// ID identifies the client in logs and, later, in rate limits and
	// usage records. It is 1 to 64 ASCII letters, digits, '-', '_', or
	// '.'.
	ID string
	// KeyHash is the SHA-256 hash of the client's API key.
	KeyHash [sha256.Size]byte
	// Disabled clients are recognized but rejected.
	Disabled bool
}

// Identity is the authenticated client of a request.
type Identity struct {
	ClientID string
}

// Authenticator maps API keys to clients. It is immutable after New and
// safe for concurrent use.
type Authenticator struct {
	clients map[[sha256.Size]byte]Client
}

// New returns an Authenticator for clients. It rejects an empty list,
// invalid or duplicate client IDs, missing key hashes, and two clients
// sharing a key.
func New(clients []Client) (*Authenticator, error) {
	if len(clients) == 0 {
		return nil, errors.New("auth: no clients")
	}
	byHash := make(map[[sha256.Size]byte]Client, len(clients))
	ids := make(map[string]bool, len(clients))
	for _, c := range clients {
		if !validClientID(c.ID) {
			return nil, fmt.Errorf("auth: client ID %q must be 1 to %d letters, digits, '-', '_', or '.'", c.ID, maxClientIDLength)
		}
		if ids[c.ID] {
			return nil, fmt.Errorf("auth: duplicate client ID %q", c.ID)
		}
		if c.KeyHash == ([sha256.Size]byte{}) {
			return nil, fmt.Errorf("auth: client %q has no key hash", c.ID)
		}
		if other, ok := byHash[c.KeyHash]; ok {
			return nil, fmt.Errorf("auth: clients %q and %q have the same key", other.ID, c.ID)
		}
		ids[c.ID] = true
		byHash[c.KeyHash] = c
	}
	return &Authenticator{clients: byHash}, nil
}

// Authenticate returns the identity of the client that owns key. It
// returns ErrInvalidKey if no client does, including for an empty key, and
// ErrDisabledKey if the client is disabled. Errors never contain the key.
//
// The key is hashed before the lookup, so the lookup's timing depends only
// on the hash, which reveals nothing usable about valid keys.
func (a *Authenticator) Authenticate(key string) (Identity, error) {
	if key == "" {
		return Identity{}, ErrInvalidKey
	}
	c, ok := a.clients[HashKey(key)]
	switch {
	case !ok:
		return Identity{}, ErrInvalidKey
	case c.Disabled:
		return Identity{}, fmt.Errorf("client %s: %w", c.ID, ErrDisabledKey)
	}
	return Identity{ClientID: c.ID}, nil
}

// HashKey returns the SHA-256 hash of key, as stored in Client.KeyHash.
func HashKey(key string) [sha256.Size]byte {
	return sha256.Sum256([]byte(key))
}

type contextKey struct{}

// NewContext returns a copy of ctx carrying id.
func NewContext(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// FromContext returns the identity stored in ctx by NewContext, if any.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(contextKey{}).(Identity)
	return id, ok
}

func validClientID(id string) bool {
	if id == "" || len(id) > maxClientIDLength {
		return false
	}
	for _, r := range id {
		switch {
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}
