package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"sync"
	"time"
)

// bridgeFile is what OpenConnector writes for this bridge. Only variable names
// appear under env; the values are fetched from OpenConnector at spawn time.
type bridgeFile struct {
	Version int                    `json:"version"`
	Token   string                 `json:"token"`
	Servers map[string]serverEntry `json:"servers"`
}

type serverEntry struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Cwd     string   `json:"cwd"`
	Env     []string `json:"env"`
}

func (e serverEntry) equal(other serverEntry) bool {
	return reflect.DeepEqual(e, other)
}

func readBridgeFile(path string) (bridgeFile, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return bridgeFile{}, err
	}
	var file bridgeFile
	if err := json.Unmarshal(content, &file); err != nil {
		return bridgeFile{}, fmt.Errorf("%s: %w", path, err)
	}
	if file.Version != 1 {
		return bridgeFile{}, fmt.Errorf("%s: unsupported version %d", path, file.Version)
	}
	if file.Token == "" {
		return bridgeFile{}, fmt.Errorf("%s: token is empty", path)
	}
	if file.Servers == nil {
		file.Servers = map[string]serverEntry{}
	}
	return file, nil
}

// fileWatcher re-reads the bridge file whenever its modification time or size
// changes and hands the new content to apply. It polls on a timer and can
// also be asked to check right now, which a request for an unknown server
// does: OpenConnector writes the file and calls the bridge in the same
// breath, sooner than the next poll.
type fileWatcher struct {
	path  string
	apply func(bridgeFile)

	mu           sync.Mutex
	lastModified time.Time
	lastSize     int64
	missing      bool
}

func newFileWatcher(path string, apply func(bridgeFile)) *fileWatcher {
	return &fileWatcher{path: path, apply: apply}
}

func (w *fileWatcher) run(interval time.Duration) {
	for {
		w.check()
		time.Sleep(interval)
	}
}

// check reads the file if it changed since the last check. A file that fails
// to parse keeps the previous content in force; a missing file is reported
// once until it appears.
func (w *fileWatcher) check() {
	w.mu.Lock()
	defer w.mu.Unlock()
	info, err := os.Stat(w.path)
	if err != nil {
		if !w.missing {
			logf("bridge file %s: %v (waiting for OpenConnector to write it)", w.path, err)
			w.missing = true
		}
		return
	}
	if info.ModTime() == w.lastModified && info.Size() == w.lastSize {
		return
	}
	w.missing = false
	w.lastModified, w.lastSize = info.ModTime(), info.Size()
	file, err := readBridgeFile(w.path)
	if err != nil {
		logf("bridge file ignored: %v", err)
		return
	}
	names := make([]string, 0, len(file.Servers))
	for name := range file.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	logf("bridge file loaded: %d server(s) %v", len(names), names)
	w.apply(file)
}
