package auth

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
)

const (
	keyA        = "key-for-team-a"
	keyDisabled = "key-for-team-old"
)

func newTestAuthenticator(t *testing.T) *Authenticator {
	t.Helper()
	a, err := New([]Client{
		{ID: "team-a", KeyHash: HashKey(keyA)},
		{ID: "team-b", KeyHash: HashKey("key-for-team-b")},
		{ID: "team-old", KeyHash: HashKey(keyDisabled), Disabled: true},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestAuthenticateValidKey(t *testing.T) {
	a := newTestAuthenticator(t)
	for key, want := range map[string]string{keyA: "team-a", "key-for-team-b": "team-b"} {
		id, err := a.Authenticate(key)
		if err != nil {
			t.Fatalf("Authenticate(%s): %v", want, err)
		}
		if id.ClientID != want {
			t.Errorf("ClientID = %q, want %q", id.ClientID, want)
		}
	}
}

func TestAuthenticateInvalidKey(t *testing.T) {
	a := newTestAuthenticator(t)
	for name, key := range map[string]string{
		"unknown":           "not-a-key",
		"empty":             "",
		"prefix of a key":   keyA[:len(keyA)-1],
		"key with a suffix": keyA + "x",
		"different case":    strings.ToUpper(keyA),
		"surrounding space": " " + keyA,
		"hash instead of key": func() string {
			h := HashKey(keyA)
			return hex.EncodeToString(h[:])
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			id, err := a.Authenticate(key)
			if !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("err = %v, want ErrInvalidKey", err)
			}
			if id != (Identity{}) {
				t.Errorf("identity = %+v, want zero", id)
			}
		})
	}
}

func TestAuthenticateDisabledKey(t *testing.T) {
	a := newTestAuthenticator(t)
	id, err := a.Authenticate(keyDisabled)
	if !errors.Is(err, ErrDisabledKey) {
		t.Fatalf("err = %v, want ErrDisabledKey", err)
	}
	if errors.Is(err, ErrInvalidKey) {
		t.Error("a disabled key must be distinguishable from an invalid one")
	}
	if !strings.Contains(err.Error(), "team-old") {
		t.Errorf("error %q does not name the client", err)
	}
	if id != (Identity{}) {
		t.Errorf("identity = %+v, want zero", id)
	}
}

func TestErrorsNeverContainKey(t *testing.T) {
	a := newTestAuthenticator(t)
	for _, key := range []string{"secret-unknown-key", keyDisabled} {
		_, err := a.Authenticate(key)
		if err == nil {
			t.Fatalf("Authenticate(%q) succeeded", key)
		}
		if strings.Contains(err.Error(), key) {
			t.Errorf("error %q contains the key", err)
		}
	}
}

func TestHashKeyIsSHA256(t *testing.T) {
	// The stored format operators produce with `printf %s "$KEY" | shasum -a 256`.
	got := HashKey("abc")
	want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if hex.EncodeToString(got[:]) != want {
		t.Errorf("HashKey(\"abc\") = %x, want %s", got, want)
	}
}

func TestNewRejectsInvalidClients(t *testing.T) {
	h := HashKey("k1")
	for name, tc := range map[string]struct {
		clients []Client
		want    string
	}{
		"no clients":      {nil, "no clients"},
		"empty ID":        {[]Client{{ID: "", KeyHash: h}}, "client ID"},
		"ID with space":   {[]Client{{ID: "team a", KeyHash: h}}, "client ID"},
		"ID with newline": {[]Client{{ID: "team\na", KeyHash: h}}, "client ID"},
		"non-ASCII ID":    {[]Client{{ID: "téam", KeyHash: h}}, "client ID"},
		"ID too long":     {[]Client{{ID: strings.Repeat("a", 65), KeyHash: h}}, "client ID"},
		"missing hash":    {[]Client{{ID: "team-a"}}, "no key hash"},
		"duplicate ID": {[]Client{
			{ID: "team-a", KeyHash: h},
			{ID: "team-a", KeyHash: HashKey("k2")},
		}, "duplicate client ID"},
		"shared key": {[]Client{
			{ID: "team-a", KeyHash: h},
			{ID: "team-b", KeyHash: h},
		}, "same key"},
		"shared key with a disabled client": {[]Client{
			{ID: "team-a", KeyHash: h},
			{ID: "team-b", KeyHash: h, Disabled: true},
		}, "same key"},
	} {
		t.Run(name, func(t *testing.T) {
			a, err := New(tc.clients)
			if err == nil {
				t.Fatalf("New succeeded: %+v", a)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestNewAcceptsBoundaryClientIDs(t *testing.T) {
	ids := []string{"a", strings.Repeat("a", 64), "Team_A-1.prod"}
	clients := make([]Client, len(ids))
	for i, id := range ids {
		clients[i] = Client{ID: id, KeyHash: HashKey(id)}
	}
	if _, err := New(clients); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestNewCopiesClients(t *testing.T) {
	clients := []Client{{ID: "team-a", KeyHash: HashKey(keyA)}}
	a, err := New(clients)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clients[0].Disabled = true
	clients[0].ID = "changed"
	id, err := a.Authenticate(keyA)
	if err != nil || id.ClientID != "team-a" {
		t.Errorf("Authenticate = %+v, %v; want team-a unaffected by later changes", id, err)
	}
}

func TestAuthenticateConcurrent(t *testing.T) {
	a := newTestAuthenticator(t)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			key, want := keyA, "team-a"
			if i%2 == 1 {
				key, want = "key-for-team-b", "team-b"
			}
			id, err := a.Authenticate(key)
			if err != nil || id.ClientID != want {
				t.Errorf("Authenticate = %+v, %v; want %s", id, err, want)
			}
		})
	}
	wg.Wait()
}

func TestContext(t *testing.T) {
	ctx := context.Background()
	if id, ok := FromContext(ctx); ok {
		t.Fatalf("FromContext on an empty context = %+v, true", id)
	}
	want := Identity{ClientID: "team-a"}
	got, ok := FromContext(NewContext(ctx, want))
	if !ok || got != want {
		t.Errorf("FromContext = %+v, %v; want %+v, true", got, ok, want)
	}
}
