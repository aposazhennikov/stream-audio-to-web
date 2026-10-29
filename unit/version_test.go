package unit_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aposazhennikov/stream-audio-to-web/version"
)

// TestVersionFromLdflags checks that the values set at build time are reported as is.
func TestVersionFromLdflags(t *testing.T) {
	oldVersion, oldCommit, oldBuild := version.Version, version.Commit, version.BuildTime
	defer func() { version.Version, version.Commit, version.BuildTime = oldVersion, oldCommit, oldBuild }()

	version.Version = "2026.10.29-c9a6b2b"
	version.Commit = "c9a6b2b0123456789abcdef0123456789abcdef0"
	version.BuildTime = "2026-10-29T10:00:00Z"

	info := version.Get()
	if info.Version != "2026.10.29-c9a6b2b" || info.Commit != version.Commit || info.BuildTime != "2026-10-29T10:00:00Z" {
		t.Fatalf("unexpected info: %+v", info)
	}
	if info.ShortCommit != "c9a6b2b" {
		t.Errorf("short commit = %q, want c9a6b2b", info.ShortCommit)
	}
	if info.StartedAt == "" {
		t.Error("startedAt is empty")
	}
}

// TestVersionWithoutLdflags checks that a local build still reports something meaningful.
func TestVersionWithoutLdflags(t *testing.T) {
	oldVersion, oldCommit, oldBuild := version.Version, version.Commit, version.BuildTime
	defer func() { version.Version, version.Commit, version.BuildTime = oldVersion, oldCommit, oldBuild }()

	version.Version, version.Commit, version.BuildTime = "", "", ""
	info := version.Get()
	if !strings.HasPrefix(info.Version, "dev") {
		t.Errorf("version = %q, want a dev version", info.Version)
	}
	if info.Commit != "" && !strings.HasSuffix(info.Version, info.ShortCommit) {
		t.Errorf("version %q does not mention commit %q", info.Version, info.ShortCommit)
	}
}

// TestVersionEndpoint checks that /version is public and returns the build information as JSON.
func TestVersionEndpoint(t *testing.T) {
	oldVersion, oldCommit := version.Version, version.Commit
	defer func() { version.Version, version.Commit = oldVersion, oldCommit }()
	version.Version = "2026.10.29-abcdef1"
	version.Commit = "abcdef1234567"

	ts := httptest.NewServer(getTestServer().Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/version")
	if err != nil {
		t.Fatalf("GET /version: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content type = %q", ct)
	}

	var info version.Info
	if decodeErr := json.NewDecoder(resp.Body).Decode(&info); decodeErr != nil {
		t.Fatalf("decode: %v", decodeErr)
	}
	if info.Version != "2026.10.29-abcdef1" || info.Commit != "abcdef1234567" || info.ShortCommit != "abcdef1" {
		t.Errorf("unexpected info: %+v", info)
	}
}
