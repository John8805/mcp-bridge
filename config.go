package main

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// bridgePayload is what OpenConnector pushes: every stdio server assigned to
// this bridge. A server's environment arrives encrypted to this bridge's key
// and is decrypted only when the server is spawned.
type bridgePayload struct {
	Version int                    `json:"version"`
	Servers map[string]serverEntry `json:"servers"`
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
