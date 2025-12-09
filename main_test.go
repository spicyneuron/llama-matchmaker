package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/spicyneuron/llama-matchmaker/config"
)

type fakeWatcher struct {
	added  []string
	closed bool
	events chan fsnotify.Event
	errs   chan error
}

func newFakeWatcher() *fakeWatcher {
	return &fakeWatcher{
		events: make(chan fsnotify.Event, 2),
		errs:   make(chan error, 1),
	}
}

func (f *fakeWatcher) Add(name string) error {
	f.added = append(f.added, name)
	return nil
}

func (f *fakeWatcher) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	close(f.events)
	close(f.errs)
	return nil
}

func (f *fakeWatcher) Events() <-chan fsnotify.Event {
	return f.events
}

func (f *fakeWatcher) Errors() <-chan error {
	return f.errs
}

func TestSetWatcherReplacesPrevious(t *testing.T) {
	origFactory := watchFactory
	origWatcher := configWatcher
	defer func() {
		watchFactory = origFactory
		configWatcher = origWatcher
		closeWatcher()
	}()

	first := newFakeWatcher()
	second := newFakeWatcher()
	created := 0

	watchFactory = func() (fileWatcher, error) {
		defer func() { created++ }()
		if created == 0 {
			return first, nil
		}
		return second, nil
	}

	if err := setWatcher([]string{"a", "b"}); err != nil {
		t.Fatalf("setWatcher initial: %v", err)
	}
	if len(first.added) != 2 {
		t.Fatalf("expected first watcher to add 2 paths, got %d", len(first.added))
	}

	if err := setWatcher([]string{"c"}); err != nil {
		t.Fatalf("setWatcher replace: %v", err)
	}
	if !first.closed {
		t.Fatalf("expected first watcher to be closed when replaced")
	}
	if len(second.added) != 1 || second.added[0] != "c" {
		t.Fatalf("expected second watcher to add new path, got %v", second.added)
	}

	closeWatcher()
	if !second.closed {
		t.Fatalf("expected second watcher to be closed on cleanup")
	}
	// Should be safe to call twice
	closeWatcher()
}

func TestReloadConfigUpdatesWatcherAndTriggersOnNewFile(t *testing.T) {
	tmpDir := t.TempDir()
	includePath := filepath.Join(tmpDir, "include.yml")
	certPath := filepath.Join(tmpDir, "cert-new.pem")
	keyPath := filepath.Join(tmpDir, "key-new.pem")
	configPath := filepath.Join(tmpDir, "config.yml")

	if err := writeFile(includePath, `
- methods: GET
  paths: /.*
  on_request:
    - merge:
        injected: true
`); err != nil {
		t.Fatalf("failed to write include: %v", err)
	}

	if err := writeFile(configPath, `
proxy:
  listen: "localhost:0"
  target: "http://example.com"
  ssl_cert: "`+filepath.Base(certPath)+`"
  ssl_key: "`+filepath.Base(keyPath)+`"
  routes:
    - include: "`+filepath.Base(includePath)+`"
`); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	origFactory := watchFactory
	origWatcher := configWatcher
	origPaths := configPaths
	origOverrides := overrides
	origStart := startAllProxiesFn
	origStop := stopAllProxiesFn
	origReload := reloadConfigFn

	defer func() {
		watchFactory = origFactory
		configWatcher = origWatcher
		configPaths = origPaths
		overrides = origOverrides
		startAllProxiesFn = origStart
		stopAllProxiesFn = origStop
		reloadConfigFn = origReload
		reloadMutex.Lock()
		if reloadTimer != nil {
			reloadTimer.Stop()
		}
		reloadMutex.Unlock()
		closeWatcher()
	}()

	var watchers []*fakeWatcher
	watchFactory = func() (fileWatcher, error) {
		fw := newFakeWatcher()
		watchers = append(watchers, fw)
		return fw, nil
	}

	startAllProxiesFn = func(*config.Config) error { return nil }
	stopAllProxiesFn = func() {}
	trigger := make(chan struct{})
	reloadConfigFn = func() {
		select {
		case <-trigger:
		default:
			close(trigger)
		}
	}

	configPaths = []string{configPath}
	overrides = config.CliOverrides{}

	reloadConfig()

	if len(watchers) != 1 {
		t.Fatalf("expected one watcher to be created, got %d", len(watchers))
	}

	added := watchers[0].added
	assertContains := func(path string) {
		for _, p := range added {
			if filepath.Base(p) == filepath.Base(path) {
				return
			}
		}
		t.Fatalf("expected watcher to include %s, got %v", path, added)
	}
	assertContains(configPath)
	assertContains(includePath)
	assertContains(certPath)
	assertContains(keyPath)

	// Simulate change on newly watched include file
	watchers[0].events <- fsnotify.Event{Name: includePath, Op: fsnotify.Write}

	select {
	case <-trigger:
	case <-time.After(2 * time.Second):
		t.Fatal("expected reloadConfigFn to be invoked after file change")
	}
}

func TestCreateServerTimeouts(t *testing.T) {
	tests := []struct {
		name      string
		timeout   time.Duration
		wantIdle  time.Duration
		wantRead  time.Duration
		wantWrite time.Duration
	}{
		{
			name:      "zero timeout",
			timeout:   0,
			wantIdle:  0,
			wantRead:  0,
			wantWrite: 0,
		},
		{
			name:      "with timeout",
			timeout:   30 * time.Second,
			wantIdle:  30 * time.Second,
			wantRead:  0,
			wantWrite: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.ProxyConfig{
				Listen:  "localhost:8080",
				Target:  "http://localhost:3000",
				Timeout: tt.timeout,
			}

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			})

			server := CreateServer(cfg, handler)

			if server.Addr != cfg.Listen {
				t.Errorf("Server.Addr = %s, want %s", server.Addr, cfg.Listen)
			}

			if server.IdleTimeout != tt.wantIdle {
				t.Errorf("Server.IdleTimeout = %v, want %v", server.IdleTimeout, tt.wantIdle)
			}

			if server.ReadTimeout != tt.wantRead {
				t.Errorf("Server.ReadTimeout = %v, want %v (must be 0 for streaming)", server.ReadTimeout, tt.wantRead)
			}

			if server.WriteTimeout != tt.wantWrite {
				t.Errorf("Server.WriteTimeout = %v, want %v (must be 0 for streaming)", server.WriteTimeout, tt.wantWrite)
			}
		})
	}
}

func TestCreateServerWithoutTLSConfig(t *testing.T) {
	cfg := config.ProxyConfig{
		Listen: "localhost:8080",
		Target: "http://localhost:3000",
	}

	server := CreateServer(cfg, http.NewServeMux())
	if server.TLSConfig != nil {
		t.Fatalf("Expected TLSConfig to be nil when no cert/key provided")
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}
