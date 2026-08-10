package update

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		candidate, current string
		want               bool
	}{
		{"0.1.1", "0.1.0", true},
		{"0.2.0", "0.1.9", true},
		{"1.0.0", "0.9.9", true},
		{"0.1.0", "0.1.0", false},
		{"0.1.0", "0.1.1", false},
		{"v0.2.0", "0.1.0", true},
		{"0.2.0-rc1", "0.1.0", true},
		{"garbage", "0.1.0", false},
		{"0.2.0", "garbage", false},
		{"", "0.1.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.candidate, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.candidate, c.current, got, c.want)
		}
	}
}

// collect drains the channel with a deadline so a hung goroutine fails
// fast instead of stalling the suite.
func collect(t *testing.T, ch <-chan string) (string, bool) {
	t.Helper()
	select {
	case v, ok := <-ch:
		return v, ok
	case <-time.After(3 * time.Second):
		t.Fatal("Check did not resolve in time")
		return "", false
	}
}

func testEnv(t *testing.T, latest string) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"latest":"` + latest + `"}`))
	}))
	t.Cleanup(srv.Close)
	oldURL, oldDir := versionURL, configDir
	versionURL = srv.URL
	tmp := t.TempDir()
	configDir = func() (string, error) { return tmp, nil }
	t.Cleanup(func() { versionURL, configDir = oldURL, oldDir })
	return &hits
}

func TestCheckReportsNewer(t *testing.T) {
	testEnv(t, "0.2.0")
	v, ok := collect(t, Check("0.1.0"))
	if !ok || v != "0.2.0" {
		t.Fatalf("got (%q, %v), want (0.2.0, true)", v, ok)
	}
}

func TestCheckQuietWhenCurrent(t *testing.T) {
	testEnv(t, "0.1.0")
	if v, ok := collect(t, Check("0.1.0")); ok {
		t.Fatalf("expected closed channel, got %q", v)
	}
}

func TestCheckUsesCache(t *testing.T) {
	hits := testEnv(t, "0.3.0")
	if v, _ := collect(t, Check("0.1.0")); v != "0.3.0" {
		t.Fatalf("first check: got %q", v)
	}
	if v, _ := collect(t, Check("0.1.0")); v != "0.3.0" {
		t.Fatalf("second check: got %q", v)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("expected 1 network hit (cache on second), got %d", n)
	}
}

func TestCheckSkipsDevAndOptOut(t *testing.T) {
	hits := testEnv(t, "9.9.9")
	if _, ok := collect(t, Check("0.1.0-dev")); ok {
		t.Fatal("dev build should never check")
	}
	t.Setenv("PULSE_NO_UPDATE_CHECK", "1")
	if _, ok := collect(t, Check("0.1.0")); ok {
		t.Fatal("opt-out should never check")
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("expected 0 network hits, got %d", n)
	}
}

func TestCheckSilentOnNetworkFailure(t *testing.T) {
	testEnv(t, "0.2.0")
	versionURL = "http://127.0.0.1:1" // nothing listens here
	if v, ok := collect(t, Check("0.1.0")); ok {
		t.Fatalf("expected silence on network failure, got %q", v)
	}
}
