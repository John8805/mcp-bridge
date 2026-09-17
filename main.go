// mcp-bridge runs stdio MCP servers on this host and exposes each one over
// Streamable HTTP for an OpenConnector instance that cannot spawn them itself.
//
// OpenConnector pushes the list of servers to PUT /config and reaches each
// server at POST /<name>, both with the bridge token. The list lives in
// memory only; until one arrives every server endpoint answers 503 with
// {"error":"unconfigured"}, which OpenConnector treats as "push again". A
// server's environment arrives encrypted to this bridge's key, the one thing
// the bridge keeps on disk, and is decrypted when the server is spawned.
package main

import (
	"crypto/ecdh"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"time"
)

type bridge struct {
	token       string
	key         *ecdh.PrivateKey
	idleTimeout time.Duration

	mu         sync.Mutex
	configured bool
	engines    map[string]*engine
}

func main() {
	var (
		listen  = flag.String("listen", "0.0.0.0:7800", "address to serve on")
		token   = flag.String("token", os.Getenv("MCP_BRIDGE_TOKEN"), "bridge token, the same value entered in OpenConnector's MCP Bridge connection (required; or MCP_BRIDGE_TOKEN)")
		keyPath = flag.String("key", envOr("MCP_BRIDGE_KEY", "mcp-bridge.key"), "file holding this bridge's private key; created on first start (or MCP_BRIDGE_KEY)")
		idle    = flag.Duration("idle", 30*time.Minute, "stop a server after this long without requests (0 keeps them)")
	)
	flag.Parse()
	if *token == "" {
		fmt.Fprintln(os.Stderr, "mcp-bridge: -token is required")
		flag.Usage()
		os.Exit(2)
	}
	key, created, err := loadOrCreateKey(*keyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcp-bridge:", err)
		os.Exit(1)
	}
	if created {
		logf("generated a new key pair in %s", *keyPath)
	}
	logf("public key (paste into the MCP Bridge connection): %s", publicKeyString(key))

	b := &bridge{token: *token, key: key, idleTimeout: *idle, engines: map[string]*engine{}}
	if *idle > 0 {
		go b.reapIdle()
	}

	// A shutdown signal is logged before it is honoured, so an exit always
	// leaves a trace; an interrupt reaching this process by accident (a child
	// sharing the console) must not end it silently.
	go func() {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt)
		for sig := range signals {
			logf("ignoring %v; stop the bridge by ending the process", sig)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", b.authenticated(b.handleHealth))
	mux.HandleFunc("PUT /config", b.authenticated(b.handleConfig))
	mux.HandleFunc("/{name}", b.authenticated(b.handleServer))
	logf("listening on http://%s", *listen)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func (b *bridge) authenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !bearerMatches(r.Header.Get("Authorization"), b.token) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "bridge token mismatch")
			return
		}
		next(w, r)
	}
}

func (b *bridge) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"publicKey": publicKeyString(b.key),
		"servers":   b.serverNames(),
	})
}

// handleConfig installs a pushed payload: new entries become engines that
// start on first use, changed entries restart on next use, removed entries
// stop now.
func (b *bridge) handleConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "cannot read body")
		return
	}
	payload, err := parseBridgePayload(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_payload", err.Error())
		return
	}
	b.apply(payload)
	names := b.serverNames()
	logf("configuration received: %d server(s) %v", len(names), names)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "servers": names})
}

func (b *bridge) apply(payload bridgePayload) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.configured = true
	for name, current := range b.engines {
		entry, keep := payload.Servers[name]
		if keep && entry.equal(current.entry) {
			continue
		}
		delete(b.engines, name)
		go func(current *engine) {
			current.mu.Lock()
			defer current.mu.Unlock()
			current.stop()
		}(current)
	}
	for name, entry := range payload.Servers {
		if _, exists := b.engines[name]; !exists {
			b.engines[name] = newEngine(name, entry)
		}
	}
}

func (b *bridge) serverNames() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	names := make([]string, 0, len(b.engines))
	for name := range b.engines {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (b *bridge) lookup(name string) (*engine, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.engines[name], b.configured
}

// handleServer is one Streamable HTTP endpoint (protocol revision 2026-07-28:
// POST only, JSON responses, no sessions). Each body carries one JSON-RPC
// message; notifications are acknowledged with 202 and, apart from the
// handshake the bridge already performed, forwarded to the server.
func (b *bridge) handleServer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	eng, configured := b.lookup(name)
	if !configured {
		writeError(w, http.StatusServiceUnavailable, "unconfigured", "the bridge has not received its server list yet")
		return
	}
	if eng == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such server: "+name)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "this bridge serves POST only")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "cannot read body")
		return
	}
	var message jsonRPCMessage
	if err := json.Unmarshal(body, &message); err != nil || message.JSONRPC != "2.0" {
		writeError(w, http.StatusBadRequest, "invalid_body", "body must be one JSON-RPC 2.0 message")
		return
	}

	eng.mu.Lock()
	defer eng.mu.Unlock()
	eng.lastUsed = time.Now()
	var initParams json.RawMessage
	if message.Method == "initialize" {
		initParams = message.Params
	}
	decrypt := func() (map[string]string, error) { return decryptEnv(b.key, name, eng.entry) }
	if err := eng.ensureStarted(decrypt, initParams); err != nil {
		logf("%s: %v", name, err)
		writeError(w, http.StatusBadGateway, "spawn_failed", err.Error())
		return
	}
	switch {
	case message.isNotification():
		if message.Method != "notifications/initialized" {
			if err := eng.notify(message.Method, message.Params); err != nil {
				writeError(w, http.StatusBadGateway, "server_failed", err.Error())
				return
			}
		}
		w.WriteHeader(http.StatusAccepted)
	case message.isRequest():
		response, err := eng.relay(message)
		if err != nil {
			logf("%s: %v", name, err)
			writeError(w, http.StatusBadGateway, "server_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, response)
	default:
		// A response from the client answers a server request; the bridge
		// never forwards those, so nothing is waiting for it.
		w.WriteHeader(http.StatusAccepted)
	}
}

// reapIdle stops servers nobody has used for idleTimeout. They restart on
// the next request, so an idle stop only costs that request the spawn time.
func (b *bridge) reapIdle() {
	for {
		time.Sleep(time.Minute)
		b.mu.Lock()
		engines := make([]*engine, 0, len(b.engines))
		for _, eng := range b.engines {
			engines = append(engines, eng)
		}
		b.mu.Unlock()
		for _, eng := range engines {
			if !eng.mu.TryLock() {
				continue
			}
			if eng.running() && time.Since(eng.lastUsed) > b.idleTimeout {
				logf("%s: idle for %s, stopping", eng.name, b.idleTimeout)
				eng.stop()
			}
			eng.mu.Unlock()
		}
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func bearerMatches(header, token string) bool {
	scheme, credential, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(credential)), []byte(token)) == 1
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": code, "message": message})
}

var logger = log.New(os.Stderr, "", log.LstdFlags)

func logf(format string, args ...any) {
	logger.Printf(format, args...)
}
