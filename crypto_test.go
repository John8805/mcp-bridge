package main

import (
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
)

// The fixture was produced by OpenConnector's encryptMcpBridgeEnv with a
// throw-away bridge key, so this test pins the two implementations together:
// key encoding, HKDF parameters, nonce and tag layout, and associated data.
type envFixture struct {
	PrivateKey string       `json:"privateKey"`
	PublicKey  string       `json:"publicKey"`
	Name       string       `json:"name"`
	Entry      serverEntry  `json:"entry"`
	Env        encryptedEnv `json:"env"`
}

func loadFixture(t *testing.T) (*ecdh.PrivateKey, envFixture) {
	t.Helper()
	content, err := os.ReadFile("testdata/env-fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture envFixture
	if err := json.Unmarshal(content, &fixture); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(fixture.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdh.P256().NewPrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	if publicKeyString(key) != fixture.PublicKey {
		t.Fatalf("public key mismatch: %s", publicKeyString(key))
	}
	fixture.Entry.Env = &fixture.Env
	return key, fixture
}

func TestDecryptEnvMatchesOpenConnector(t *testing.T) {
	key, fixture := loadFixture(t)
	env, err := decryptEnv(key, fixture.Name, fixture.Entry)
	if err != nil {
		t.Fatal(err)
	}
	if env["API_KEY"] != "s3cret" || env["B"] != "b" || len(env) != 2 {
		t.Fatalf("unexpected env %v", env)
	}
}

func TestDecryptEnvRejectsChangedEntry(t *testing.T) {
	key, fixture := loadFixture(t)
	cases := map[string]serverEntry{
		"command": {Command: "curl", Args: fixture.Entry.Args, Cwd: fixture.Entry.Cwd, Env: fixture.Entry.Env},
		"args":    {Command: fixture.Entry.Command, Args: []string{"-y"}, Cwd: fixture.Entry.Cwd, Env: fixture.Entry.Env},
		"cwd":     {Command: fixture.Entry.Command, Args: fixture.Entry.Args, Cwd: "", Env: fixture.Entry.Env},
	}
	for label, entry := range cases {
		if _, err := decryptEnv(key, fixture.Name, entry); err == nil {
			t.Errorf("%s: changed entry still decrypted", label)
		}
	}
	if _, err := decryptEnv(key, "other", fixture.Entry); err == nil {
		t.Error("renamed entry still decrypted")
	}
}

func TestDecryptEnvWithoutEnvIsEmpty(t *testing.T) {
	key, fixture := loadFixture(t)
	env, err := decryptEnv(key, fixture.Name, serverEntry{Command: "node"})
	if err != nil || len(env) != 0 {
		t.Fatalf("expected empty env, got %v %v", env, err)
	}
}
