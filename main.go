// mcp-bridge runs stdio MCP servers on this host and exposes each one over
// Streamable HTTP for an OpenConnector instance that cannot spawn them itself.
//
// OpenConnector writes the list of servers to a file this bridge watches (see
// docs/mcp-bridge.md in the OpenConnector repository). Each server answers at
// POST /<name>. Before spawning a server the bridge asks OpenConnector for the
// server's environment, presenting the file's token and the request token
// OpenConnector attached to the request that needs the server.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const requestTokenHeader = "X-Open-Connector-Request-Token"

type bridge struct {
	connectorURL string
	idleTimeout  time.Duration
	client       *http.Client

	mu      sync.Mutex
	token   string
	engines map[string]*engine
	watcher *fileWatcher
}

func main() {
	var (
		filePath     = flag.String("file", "", "bridge file written by OpenConnector (required)")
		listen       = flag.String("listen", "0.0.0.0:7800", "address to serve on")
		connectorURL = flag.String("connector", "http://127.0.0.1:3010", "OpenConnector base URL, for environment lookups")
		idle         = flag.Duration("idle", 30*time.Minute, "stop a server after this long without requests (0 keeps them)")
		poll         = flag.Duration("poll", time.Second, "how often to check the bridge file")
	)
	flag.Parse()
	if *filePath == "" {
		fmt.Fprintln(os.Stderr, "mcp-bridge: -file is required")
		flag.Usage()
		os.Exit(2)
	}

	b := &bridge{
		connectorURL: strings.TrimRight(*connectorURL, "/"),
		idleTimeout:  *idle,
		client:       &http.Client{Timeout: 15 * time.Second},
		engines:      map[string]*engine{},
	}
	b.watcher = newFileWatcher(*filePath, b.apply)
	go b.watcher.run(*poll)
	if *idle > 0 {
		go b.reapIdle()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "servers": b.serverNames()})
	})
	mux.HandleFunc("/{name}", b.handleServer)
	logf("listening on http://%s, OpenConnector at %s", *listen, b.connectorURL)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// apply installs a freshly read bridge file: new entries become engines that
// start on first use, changed entries restart on next use, removed entries
// stop now.
func (b *bridge) apply(file bridgeFile) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.token = file.Token
	for name, current := range b.engines {
		entry, keep := file.Servers[name]
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
	for name, entry := range file.Servers {
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
	return names
}

func (b *bridge) lookup(name string) (*engine, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.engines[name], b.token
}

// handleServer is one Streamable HTTP endpoint (protocol revision 2026-07-28:
// POST only, JSON responses, no sessions). Each body carries one JSON-RPC
// message; notifications are acknowledged with 202 and, apart from the
// handshake the bridge already performed, forwarded to the server.
func (b *bridge) handleServer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	eng, token := b.lookup(name)
	if eng == nil {
		// OpenConnector writes the file and calls the bridge right away, so a
		// name the last poll did not know may already be on disk.
		b.watcher.check()
		eng, token = b.lookup(name)
	}
	if token == "" {
		writeError(w, http.StatusServiceUnavailable, "bridge file not loaded yet")
		return
	}
	if !bearerMatches(r.Header.Get("Authorization"), token) {
		writeError(w, http.StatusUnauthorized, "bridge token mismatch")
		return
	}
	if eng == nil {
		writeError(w, http.StatusNotFound, "no such server: "+name)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "this bridge serves POST only")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body")
		return
	}
	var message jsonRPCMessage
	if err := json.Unmarshal(body, &message); err != nil || message.JSONRPC != "2.0" {
		writeError(w, http.StatusBadRequest, "body must be one JSON-RPC 2.0 message")
		return
	}
	requestToken := r.Header.Get(requestTokenHeader)
	fetchEnv := func() (map[string]string, error) { return b.fetchEnvironment(name, token, requestToken, eng.entry.Env) }

	eng.mu.Lock()
	defer eng.mu.Unlock()
	eng.lastUsed = time.Now()
	var initParams json.RawMessage
	if message.Method == "initialize" {
		initParams = message.Params
	}
	if err := eng.ensureStarted(fetchEnv, initParams); err != nil {
		logf("%s: %v", name, err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	switch {
	case message.isNotification():
		if message.Method != "notifications/initialized" {
			if err := eng.notify(message.Method, message.Params); err != nil {
				writeError(w, http.StatusBadGateway, err.Error())
				return
			}
		}
		w.WriteHeader(http.StatusAccepted)
	case message.isRequest():
		response, err := eng.relay(message)
		if err != nil {
			logf("%s: %v", name, err)
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, response)
	default:
		// A response from the client answers a server request; the bridge
		// never forwards those, so nothing is waiting for it.
		w.WriteHeader(http.StatusAccepted)
	}
}

// fetchEnvironment asks OpenConnector for the server's environment values.
// Skipped entirely when the entry names no variables, so a server with no
// secrets starts even from a request that carries no request token.
func (b *bridge) fetchEnvironment(name, token, requestToken string, names []string) (map[string]string, error) {
	if len(names) == 0 {
		return map[string]string{}, nil
	}
	if requestToken == "" {
		return nil, errors.New("the request carries no " + requestTokenHeader + " header, so the environment cannot be fetched")
	}
	endpoint := b.connectorURL + "/api/mcp-bridge/servers/" + url.PathEscape(name) + "/env"
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(requestTokenHeader, requestToken)
	response, err := b.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("OpenConnector answered %d: %s", response.StatusCode, strings.TrimSpace(string(detail)))
	}
	var payload struct {
		Env map[string]string `json:"env"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("OpenConnector answered with invalid JSON: %w", err)
	}
	return payload.Env, nil
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

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

var logger = log.New(os.Stderr, "", log.LstdFlags)

func logf(format string, args ...any) {
	logger.Printf(format, args...)
}
