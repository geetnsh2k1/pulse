package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"pulse/internal/store"
)

// RunInfo is written to .pulse/runtime/engine.json while an engine runs, so
// other pulse processes (CLI stop/list, the desktop app) can find it. Whether
// an engine is *actually* alive is always decided by probing /health, never
// by trusting this file — leftover files from crashes are cleaned up lazily.
type RunInfo struct {
	PID       int       `json:"pid"`
	Addr      string    `json:"addr"`              // control API base URL
	APIAddr   string    `json:"apiAddr,omitempty"` // local API base URL (when http triggers exist)
	AWSAddr   string    `json:"awsAddr,omitempty"` // local AWS endpoint (the façade)
	Project   string    `json:"project"`
	Root      string    `json:"root"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"startedAt"`
}

func runFilePath(root string) string {
	return filepath.Join(root, store.Dir, "runtime", "engine.json")
}

func WriteRunInfo(root string, info RunInfo) error {
	p := runFilePath(root)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

func ReadRunInfo(root string) (*RunInfo, error) {
	b, err := os.ReadFile(runFilePath(root))
	if err != nil {
		return nil, err
	}
	var info RunInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func RemoveRunInfo(root string) { _ = os.Remove(runFilePath(root)) }

// Probe reports whether the engine described by info is alive, by asking
// its /health endpoint — the one check that is truthful on every OS.
func Probe(info *RunInfo) bool {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(info.Addr + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var h struct {
		Status  string `json:"status"`
		Project string `json:"project"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return false
	}
	return h.Status == "ok" && h.Project == info.Project
}

// Current returns the live engine for root, lazily removing stale runfiles
// left behind by crashes or unclean exits.
func Current(root string) (*RunInfo, bool) {
	info, err := ReadRunInfo(root)
	if err != nil {
		return nil, false
	}
	if !Probe(info) {
		RemoveRunInfo(root)
		return nil, false
	}
	return info, true
}

// RequestShutdown asks a live engine to stop and waits for it to disappear.
func RequestShutdown(info *RunInfo, wait time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post(info.Addr+"/api/shutdown", "application/json", nil)
	if err != nil {
		return fmt.Errorf("engine unreachable: %w", err)
	}
	resp.Body.Close()

	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if !Probe(info) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("engine did not exit in time — kill it manually and rerun")
}
