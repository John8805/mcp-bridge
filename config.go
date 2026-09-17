package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"
)

// pushMaxSkew bounds how far a push's issuedAt may sit from this clock; a
// push outside it is stale or replayed. Pushes must also arrive in order.
const pushMaxSkew = 10 * time.Minute

// parseConnectorKey reads the base64 Ed25519 public key OpenConnector shows
// as its push-signing key.
func parseConnectorKey(value string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("-connector-key must be the base64 Ed25519 public key OpenConnector shows on its MCP page")
	}
	return ed25519.PublicKey(raw), nil
}

// verifyPush checks that the body was signed by OpenConnector and that its
// timestamp is fresh and not older than the last accepted push; a repeat of
// the same push is harmless and stays accepted.
func verifyPush(key ed25519.PublicKey, body []byte, signature string, issuedAt, lastIssuedAt int64, now time.Time) error {
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("missing or malformed signature")
	}
	if !ed25519.Verify(key, body, sig) {
		return errors.New("signature does not verify against the connector key")
	}
	issued := time.UnixMilli(issuedAt)
	if d := now.Sub(issued); d > pushMaxSkew || d < -pushMaxSkew {
		return fmt.Errorf("issuedAt %s is outside the accepted window", issued.UTC().Format(time.RFC3339))
	}
	if issuedAt < lastIssuedAt {
		return errors.New("push is older than the one already applied")
	}
	return nil
}

// bridgePayload is what OpenConnector pushes: every stdio server assigned to
// this bridge. A server's environment arrives encrypted to this bridge's key
// and is decrypted only when the server is spawned.
type bridgePayload struct {
	Version  int                    `json:"version"`
	IssuedAt int64                  `json:"issuedAt"`
	Servers  map[string]serverEntry `json:"servers"`
}

type serverEntry struct {
	Command string        `json:"command"`
	Args    []string      `json:"args"`
	Cwd     string        `json:"cwd"`
	Env     *encryptedEnv `json:"env"`
}

type encryptedEnv struct {
	EphemeralPublicKey string `json:"ephemeralPublicKey"`
	Nonce              string `json:"nonce"`
	Ciphertext         string `json:"ciphertext"`
}

func (e serverEntry) equal(other serverEntry) bool {
	return reflect.DeepEqual(e, other)
}

func parseBridgePayload(body []byte) (bridgePayload, error) {
	var payload bridgePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return bridgePayload{}, err
	}
	if payload.Version != 1 {
		return bridgePayload{}, fmt.Errorf("unsupported payload version %d", payload.Version)
	}
	if payload.Servers == nil {
		payload.Servers = map[string]serverEntry{}
	}
	for name, entry := range payload.Servers {
		if name == "" || entry.Command == "" {
			return bridgePayload{}, fmt.Errorf("server %q: command is required", name)
		}
	}
	return payload, nil
}
