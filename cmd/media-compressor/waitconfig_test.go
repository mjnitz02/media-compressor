package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A config that is already there is read immediately -- the waiting path must
// not add a delay to the normal case.
func TestWaitForConfigReturnsAnExistingConfigAtOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeUsableConfig(t, path, dir)

	var out bytes.Buffer
	started := time.Now()
	cfg, err := waitForConfig(context.Background(), path, "", &out)
	if err != nil {
		t.Fatalf("waitForConfig: %v", err)
	}
	if took := time.Since(started); took > configPollInterval {
		t.Errorf("took %v; an existing config should not wait for a tick", took)
	}
	if len(cfg.Libraries) != 1 {
		t.Errorf("got %d libraries, want 1", len(cfg.Libraries))
	}
	if out.Len() != 0 {
		t.Errorf("nothing should be said when there is nothing wrong, got %q", out.String())
	}
}

// The point of the whole change: the daemon does not exit because the config
// is not there yet, and it picks the file up on its own without a restart.
func TestWaitForConfigPicksUpAConfigThatAppears(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	go func() {
		time.Sleep(configPollInterval / 2)
		writeUsableConfig(t, path, dir)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var out bytes.Buffer
	cfg, err := waitForConfig(ctx, path, "", &out)
	if err != nil {
		t.Fatalf("waitForConfig: %v", err)
	}
	if len(cfg.Libraries) != 1 {
		t.Errorf("got %d libraries, want 1", len(cfg.Libraries))
	}
	if !strings.Contains(out.String(), "waiting for a usable configuration") {
		t.Errorf("the wait should be announced, got %q", out.String())
	}
	if !strings.Contains(out.String(), "no need to restart") {
		t.Errorf("the operator should be told not to restart, got %q", out.String())
	}
}

// A config that arrives with a typo in it must not end the wait. Exiting on it
// would be the same restart-backoff trap, one step later.
func TestWaitForConfigKeepsWaitingThroughABadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("libraries: [oh dear\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(configPollInterval / 2)
		writeUsableConfig(t, path, dir)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var out bytes.Buffer
	if _, err := waitForConfig(ctx, path, "", &out); err != nil {
		t.Fatalf("waitForConfig: %v", err)
	}
}

// Ctrl-C and SIGTERM have to get out of the wait, or a container that was
// never configured could not be stopped.
func TestWaitForConfigStopsWhenTheContextIsCancelled(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	var out bytes.Buffer
	_, err := waitForConfig(ctx, filepath.Join(dir, "config.yaml"), "", &out)
	if err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// The holding page has to say why, and /healthz has to fail -- a health check
// that passed here would report a healthy container that is encoding nothing.
func TestWaitingServerPageAndHealth(t *testing.T) {
	w := &waitingServer{path: "/config/config.yaml"}
	w.setErr(fmt.Errorf("there is no configuration at /config/config.yaml yet"))

	addr := freeAddr(t)
	if err := w.start(addr); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer w.stop()

	body, code := get(t, "http://"+addr+"/")
	if code != http.StatusServiceUnavailable {
		t.Errorf("page status = %d, want 503", code)
	}
	if !strings.Contains(body, "there is no configuration") {
		t.Errorf("the page should carry the reason, got %q", body)
	}
	if !strings.Contains(body, "No media has been") {
		t.Errorf("the page should say nothing was touched, got %q", body)
	}

	body, code = get(t, "http://"+addr+"/healthz")
	if code != http.StatusServiceUnavailable {
		t.Errorf("healthz status = %d, want 503 while unconfigured", code)
	}
	if !strings.Contains(body, "waiting for a usable configuration") {
		t.Errorf("healthz should say why, got %q", body)
	}
}

// The holding page must let go of the port, or the real web UI cannot bind it
// when the config finally arrives.
func TestWaitingServerReleasesThePort(t *testing.T) {
	addr := freeAddr(t)
	w := &waitingServer{path: "x"}
	if err := w.start(addr); err != nil {
		t.Fatalf("start: %v", err)
	}
	w.stop()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the port was not released: %v", err)
	}
	ln.Close()
}

// loadUsableConfig folds every reason the daemon could not start into one
// error, so the holding page has something specific to show.
func TestLoadUsableConfigReportsWhy(t *testing.T) {
	dir := t.TempDir()

	if _, err := loadUsableConfig(filepath.Join(dir, "nope.yaml")); err == nil ||
		!strings.Contains(err.Error(), "no configuration at") {
		t.Errorf("missing file: got %v", err)
	}

	missingMount := filepath.Join(dir, "missing-mount.yaml")
	writeConfigWithLibraryPath(t, missingMount, dir, filepath.Join(dir, "not-mounted"))
	if _, err := loadUsableConfig(missingMount); err == nil {
		t.Error("a library path that does not exist should not be usable")
	}
}

func writeUsableConfig(t *testing.T, path, dir string) {
	t.Helper()
	writeConfigWithLibraryPath(t, path, dir, dir)
}

func writeConfigWithLibraryPath(t *testing.T, path, dir, libPath string) {
	t.Helper()
	body := fmt.Sprintf(`
defaults:
  profile: standard
  container: mkv
  workers: { remux: 1, encode: 1 }
profiles:
  standard:
    video:
      target_codec: hevc
      encoder: libx265
      leave_alone: [hevc]
      source_bitrate_basis: container
      bitrate:
        tiers: [{ above: 0, divisor: 1.5 }]
        min_multiplier: 0.7
        max_multiplier: 1.3
        floor_kbps: 3000
    audio: { multichannel_to: ac3, multichannel_threshold: 6, keep_languages: [eng], keep_untagged: true }
    subtitles: { keep_languages: [eng] }
scanner:
  extensions: [mkv]
  min_age_seconds: 0
paths:
  work_dir: %s
  database: %s
libraries:
  - name: test
    profile: standard
    notify_plex: false
    paths: [%s]
`, filepath.Join(dir, "work"), filepath.Join(dir, "mc.db"), libPath)

	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Written whole rather than appended to: the daemon may be reading it on
	// any tick, and a half-written file would be a parse error that the test
	// would have to tolerate rather than a clean before/after.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func get(t *testing.T, url string) (string, int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	if _, err := b.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	return b.String(), resp.StatusCode
}
