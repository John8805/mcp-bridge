package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

const hkdfInfo = "open-connector mcp bridge env v1"

// loadOrCreateKey reads the bridge's P-256 private key from path, creating
// it on first start. The key is the only thing the bridge keeps on disk; it
// must stay the same across restarts because OpenConnector holds the matching
// public key.
func loadOrCreateKey(path string) (*ecdh.PrivateKey, bool, error) {
	if content, err := os.ReadFile(path); err == nil {
		raw, err := base64.StdEncoding.DecodeString(string(trimSpace(content)))
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", path, err)
		}
		key, err := ecdh.P256().NewPrivateKey(raw)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", path, err)
		}
		return key, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, false, err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key.Bytes())+"\n"), 0o600); err != nil {
		return nil, false, err
	}
	return key, true, nil
}

func trimSpace(content []byte) []byte {
	start, end := 0, len(content)
	for start < end && (content[start] == ' ' || content[start] == '\n' || content[start] == '\r' || content[start] == '\t') {
		start++
	}
	for end > start && (content[end-1] == ' ' || content[end-1] == '\n' || content[end-1] == '\r' || content[end-1] == '\t') {
		end--
	}
	return content[start:end]
}

// publicKeyString is what the operator pastes into OpenConnector: the
// uncompressed P-256 point, base64.
func publicKeyString(key *ecdh.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(key.PublicKey().Bytes())
}

// decryptEnv opens the environment OpenConnector encrypted to this bridge.
// The associated data is rebuilt from the entry as received, so an entry
// whose command changed since encryption cannot use the environment.
func decryptEnv(key *ecdh.PrivateKey, name string, entry serverEntry) (map[string]string, error) {
	if entry.Env == nil {
		return map[string]string{}, nil
	}
	ephemeralRaw, err := base64.StdEncoding.DecodeString(entry.Env.EphemeralPublicKey)
	if err != nil {
		return nil, fmt.Errorf("ephemeral key: %w", err)
	}
	ephemeral, err := ecdh.P256().NewPublicKey(ephemeralRaw)
	if err != nil {
		return nil, fmt.Errorf("ephemeral key: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(entry.Env.Nonce)
	if err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(entry.Env.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("ciphertext: %w", err)
	}
	shared, err := key.ECDH(ephemeral)
	if err != nil {
		return nil, err
	}
	aesKey, err := hkdf.Key(sha256.New, shared, nil, hkdfInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, associatedData(name, entry))
	if err != nil {
		return nil, errors.New("environment does not decrypt: wrong bridge key, or the entry changed since it was encrypted")
	}
	var env map[string]string
	if err := json.Unmarshal(plaintext, &env); err != nil {
		return nil, fmt.Errorf("environment is not a JSON object of strings: %w", err)
	}
	return env, nil
}

// associatedData mirrors OpenConnector's mcpBridgeEnvAssociatedData: each
// field as a 4-byte big-endian length followed by its UTF-8 bytes, in the
// order name, command, cwd, then every argument.
func associatedData(name string, entry serverEntry) []byte {
	fields := append([]string{name, entry.Command, entry.Cwd}, entry.Args...)
	var out []byte
	for _, field := range fields {
		out = binary.BigEndian.AppendUint32(out, uint32(len(field)))
		out = append(out, field...)
	}
	return out
}
