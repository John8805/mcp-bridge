package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// The fixture was signed by OpenConnector's signMcpBridgePush with a key
// derived from a throw-away encryption key, pinning key encoding and the
// signed bytes between the two implementations.
type pushFixture struct {
	PublicKey string `json:"publicKey"`
	Body      string `json:"body"`
	Signature string `json:"signature"`
}

func loadPushFixture(t *testing.T) pushFixture {
	t.Helper()
	content, err := os.ReadFile("testdata/push-fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture pushFixture
	if err := json.Unmarshal(content, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestVerifyPushAcceptsOpenConnectorSignature(t *testing.T) {
	fixture := loadPushFixture(t)
	key, err := parseConnectorKey(fixture.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := parseBridgePayload([]byte(fixture.Body))
	if err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(payload.IssuedAt).Add(time.Minute)
	if err := verifyPush(key, []byte(fixture.Body), fixture.Signature, payload.IssuedAt, 0, now); err != nil {
		t.Fatal(err)
	}
	// The same push again is harmless.
	if err := verifyPush(key, []byte(fixture.Body), fixture.Signature, payload.IssuedAt, payload.IssuedAt, now); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyPushRejectsTamperingStalenessAndReplay(t *testing.T) {
	fixture := loadPushFixture(t)
	key, _ := parseConnectorKey(fixture.PublicKey)
	payload, _ := parseBridgePayload([]byte(fixture.Body))
	fresh := time.UnixMilli(payload.IssuedAt).Add(time.Minute)

	tampered := []byte(fixture.Body[:len(fixture.Body)-2] + " }")
	if err := verifyPush(key, tampered, fixture.Signature, payload.IssuedAt, 0, fresh); err == nil {
		t.Error("tampered body accepted")
	}
	if err := verifyPush(key, []byte(fixture.Body), "", payload.IssuedAt, 0, fresh); err == nil {
		t.Error("missing signature accepted")
	}
	if err := verifyPush(key, []byte(fixture.Body), fixture.Signature, payload.IssuedAt, 0, fresh.Add(time.Hour)); err == nil {
		t.Error("stale push accepted")
	}
	if err := verifyPush(key, []byte(fixture.Body), fixture.Signature, payload.IssuedAt, payload.IssuedAt+1, fresh); err == nil {
		t.Error("older push accepted after a newer one")
	}
	if _, err := parseConnectorKey("not-a-key"); err == nil {
		t.Error("malformed connector key accepted")
	}
}
