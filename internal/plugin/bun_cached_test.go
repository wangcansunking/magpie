package plugin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCachedBunContextCancelsDuringAnotherDownload(t *testing.T) {
	t.Setenv("MAGPIE_BUN", "")
	bunMu.Lock()
	defer bunMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	started, finished := make(chan struct{}), make(chan struct{})
	var got string
	var err error
	go func() { close(started); got, err = CachedBunContext(ctx); close(finished) }()
	<-started
	cancel()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("cache lookup waited for an unrelated runtime download after cancellation")
	}
	if got != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("lookup after cancel: %q %v", got, err)
	}
}

func TestCachedBunNeverDownloadsOrRuns(t *testing.T) {
	bunHome(t, BunVersion, time.Now())
	asked := bunReleases(t, []byte("not executable"))
	if path := CachedBun(); path != "" {
		t.Fatalf("missing runtime resolved to %q", path)
	}
	path := bunExeOf(BunVersion)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	// These bytes cannot run. Discovery must return the cache path without
	// launching the runtime or asking the release server to repair it.
	if err := os.WriteFile(path, []byte("not executable"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := CachedBun(); got != path {
		t.Fatalf("cached runtime = %q, want %q", got, path)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := CachedBun(); got != "" || asked.Load() != 0 {
		t.Fatalf("removed runtime = %q; downloads = %d", got, asked.Load())
	}
}
