// Package update implements pulse's once-a-day update check: a plain GET
// for a static JSON file, no payload, no identifiers. Offline, slow, or
// failing networks are silent — the check must never cost the user
// anything. Opt out with PULSE_NO_UPDATE_CHECK=1.
package update

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Swappable for tests.
var (
	versionURL = "https://getpulse.run/version.json"
	configDir  = os.UserConfigDir
	now        = time.Now
)

const cacheTTL = 24 * time.Hour

type state struct {
	CheckedAt time.Time `json:"checkedAt"`
	Latest    string    `json:"latest"`
}

// Check returns a buffered channel that yields the newer version string if
// one exists, then closes. It never blocks the caller and never reports
// errors: a failed check is indistinguishable from being up to date.
// Dev builds ("-dev" versions) and opted-out users get a closed channel.
func Check(current string) <-chan string {
	ch := make(chan string, 1)
	if os.Getenv("PULSE_NO_UPDATE_CHECK") != "" || strings.Contains(current, "-dev") || current == "" {
		close(ch)
		return ch
	}
	go func() {
		defer close(ch)
		st, ok := readState()
		if !ok || now().Sub(st.CheckedAt) > cacheTTL {
			latest, err := fetchLatest()
			if err != nil {
				return
			}
			st = state{CheckedAt: now(), Latest: latest}
			writeState(st) // best-effort; a failed write just re-checks next run
		}
		if Newer(st.Latest, current) {
			ch <- st.Latest
		}
	}()
	return ch
}

func fetchLatest() (string, error) {
	client := &http.Client{Timeout: 2500 * time.Millisecond}
	resp, err := client.Get(versionURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v struct {
		Latest string `json:"latest"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	return strings.TrimSpace(v.Latest), nil
}

func statePath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "pulse", "update.json"), nil
}

func readState() (state, bool) {
	p, err := statePath()
	if err != nil {
		return state{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return state{}, false
	}
	var st state
	if json.Unmarshal(b, &st) != nil || st.Latest == "" {
		return state{}, false
	}
	return st, true
}

func writeState(st state) {
	p, err := statePath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	b, _ := json.Marshal(st)
	_ = os.WriteFile(p, b, 0o644)
}

// Newer reports whether candidate is a strictly newer semver than current.
// Leading "v" and any pre-release suffix ("-rc1") are tolerated; anything
// unparsable is treated as not-newer — the check stays quiet on garbage.
func Newer(candidate, current string) bool {
	ca, ok1 := parse(candidate)
	cu, ok2 := parse(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < 3; i++ {
		if ca[i] != cu[i] {
			return ca[i] > cu[i]
		}
	}
	return false
}

func parse(v string) ([3]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}
