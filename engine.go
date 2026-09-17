package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// jsonRPCMessage is the subset of a JSON-RPC 2.0 message the bridge inspects.
// Params and Result stay raw; the bridge relays them without interpretation.
type jsonRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

func (m jsonRPCMessage) isRequest() bool      { return m.Method != "" && len(m.ID) > 0 }
func (m jsonRPCMessage) isNotification() bool { return m.Method != "" && len(m.ID) == 0 }
func (m jsonRPCMessage) isResponse() bool     { return m.Method == "" && len(m.ID) > 0 }

// engine is one stdio MCP server: the child process, the pipes, and the
// initialize result the child gave, replayed to every client that connects
// through the bridge. One exchange at a time, so the whole engine is locked
// while a request is in flight.
type engine struct {
	name  string
	entry serverEntry

	mu         sync.Mutex
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdout     *bufio.Reader
	nextID     int64
	initResult json.RawMessage
	lastUsed   time.Time
	dead       chan struct{}
}

func newEngine(name string, entry serverEntry) *engine {
	return &engine{name: name, entry: entry}
}

// ensureStarted spawns the child if it is not running, decrypting its
// environment through decryptEnv first. initParams are the client's
// initialize params when the spawn is caused by an initialize request;
// otherwise the bridge initializes with its own.
func (e *engine) ensureStarted(decryptEnv func() (map[string]string, error), initParams json.RawMessage) error {
	if e.running() {
		return nil
	}
	env, err := decryptEnv()
	if err != nil {
		return fmt.Errorf("environment for %s: %w", e.name, err)
	}
	if err := e.start(env); err != nil {
		return fmt.Errorf("start %s: %w", e.name, err)
	}
	if err := e.initialize(initParams); err != nil {
		e.stop()
		return fmt.Errorf("initialize %s: %w", e.name, err)
	}
	return nil
}

func (e *engine) running() bool {
	if e.cmd == nil {
		return false
	}
	select {
	case <-e.dead:
		return false
	default:
		return true
	}
}

func (e *engine) start(env map[string]string) error {
	cmd := exec.Command(e.entry.Command, e.entry.Args...)
	cmd.Dir = e.entry.Cwd
	cmd.Env = mergeEnvironment(os.Environ(), env)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	dead := make(chan struct{})
	go relayStderr(e.name, stderr)
	go func() {
		err := cmd.Wait()
		close(dead)
		logf("%s: exited: %v", e.name, err)
	}()
	e.cmd, e.stdin, e.stdout, e.dead = cmd, stdin, bufio.NewReaderSize(stdout, 1<<20), dead
	e.nextID, e.initResult = 0, nil
	logf("%s: started pid %d (%s %s)", e.name, cmd.Process.Pid, e.entry.Command, strings.Join(e.entry.Args, " "))
	return nil
}

// stop closes stdin so the child can exit on its own, then kills it if it
// has not within two seconds.
func (e *engine) stop() {
	if e.cmd == nil {
		return
	}
	_ = e.stdin.Close()
	select {
	case <-e.dead:
	case <-time.After(2 * time.Second):
		_ = e.cmd.Process.Kill()
		<-e.dead
	}
	e.cmd = nil
}

var defaultInitializeParams = json.RawMessage(`{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"mcp-bridge","version":"1.0.0"}}`)

func (e *engine) initialize(params json.RawMessage) error {
	if len(params) == 0 {
		params = defaultInitializeParams
	}
	response, err := e.exchange("initialize", params)
	if err != nil {
		return err
	}
	if len(response.Error) > 0 {
		return fmt.Errorf("server refused initialize: %s", response.Error)
	}
	e.initResult = response.Result
	return e.notify("notifications/initialized", nil)
}

// relay forwards one client request and returns the child's response with
// the client's id restored. initialize is answered from the cached handshake:
// the child was initialized once at spawn, and a second initialize is
// something many servers reject.
func (e *engine) relay(message jsonRPCMessage) (jsonRPCMessage, error) {
	if message.Method == "initialize" {
		return jsonRPCMessage{JSONRPC: "2.0", ID: message.ID, Result: e.initResult}, nil
	}
	response, err := e.exchange(message.Method, message.Params)
	if err != nil {
		return jsonRPCMessage{}, err
	}
	response.ID = message.ID
	return response, nil
}

// exchange writes one request with a bridge-owned id and reads until the
// matching response arrives. Notifications from the child are logged and
// skipped; requests from the child (sampling, elicitation) are refused, since
// nothing behind the bridge can answer them.
func (e *engine) exchange(method string, params json.RawMessage) (jsonRPCMessage, error) {
	e.nextID++
	id := e.nextID
	request := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if len(params) > 0 {
		request["params"] = params
	}
	if err := e.write(request); err != nil {
		return jsonRPCMessage{}, err
	}
	expected := fmt.Sprint(id)
	for {
		line, err := e.stdout.ReadBytes('\n')
		if err != nil && len(line) == 0 {
			return jsonRPCMessage{}, fmt.Errorf("server closed its output: %w", err)
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var message jsonRPCMessage
		if err := json.Unmarshal(line, &message); err != nil {
			logf("%s: unparseable line from server: %s", e.name, strings.TrimSpace(string(line)))
			continue
		}
		switch {
		case message.isResponse() && string(message.ID) == expected:
			return message, nil
		case message.isRequest():
			_ = e.write(map[string]any{
				"jsonrpc": "2.0", "id": message.ID,
				"error": map[string]any{"code": -32601, "message": "the bridge does not answer server requests"},
			})
		case message.isNotification():
			logf("%s: notification %s", e.name, message.Method)
		}
	}
}

func (e *engine) notify(method string, params json.RawMessage) error {
	notification := map[string]any{"jsonrpc": "2.0", "method": method}
	if len(params) > 0 {
		notification["params"] = params
	}
	return e.write(notification)
}

func (e *engine) write(message any) error {
	line, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if _, err := e.stdin.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write to server: %w", err)
	}
	return nil
}

func relayStderr(name string, stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		logf("%s: %s", name, scanner.Text())
	}
}

// mergeEnvironment lays the fetched variables over the bridge's own
// environment. On Windows variable names compare case-insensitively, so an
// override replaces an inherited entry whatever its casing.
func mergeEnvironment(inherited []string, overrides map[string]string) []string {
	merged := make([]string, 0, len(inherited)+len(overrides))
	for _, entry := range inherited {
		name, _, _ := strings.Cut(entry, "=")
		if _, replaced := lookupFold(overrides, name); !replaced {
			merged = append(merged, entry)
		}
	}
	for name, value := range overrides {
		merged = append(merged, name+"="+value)
	}
	return merged
}

func lookupFold(values map[string]string, name string) (string, bool) {
	for key, value := range values {
		if strings.EqualFold(key, name) {
			return value, true
		}
	}
	return "", false
}
