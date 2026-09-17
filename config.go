package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
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

// watchBridgeFile re-reads the file whenever its modification time changes and
// hands the new content to apply. A file that fails to parse keeps the
// previous content in force; a missing file is reported once until it appears.
func watchBridgeFile(path string, interval time.Duration, apply func(bridgeFile)) {
	var lastModified time.Time
	var lastSize int64
	missing := false
	for {
		info, err := os.Stat(path)
		switch {
		case err != nil:
			if !missing {
				logf("bridge file %s: %v (waiting for OpenConnector to write it)", path, err)
				missing = true
			}
		case info.ModTime() != lastModified || info.Size() != lastSize:
			missing = false
			lastModified, lastSize = info.ModTime(), info.Size()
			file, err := readBridgeFile(path)
			if err != nil {
				logf("bridge file ignored: %v", err)
				break
			}
			names := make([]string, 0, len(file.Servers))
			for name := range file.Servers {
				names = append(names, name)
			}
			sort.Strings(names)
			logf("bridge file loaded: %d server(s) %v", len(names), names)
			apply(file)
		}
		time.Sleep(interval)
	}
}
