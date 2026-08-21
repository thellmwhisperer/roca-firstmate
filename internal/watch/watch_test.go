package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPollingFallbackEmitsChangedMarkdown(t *testing.T) {
	root := t.TempDir()
	markdown := filepath.Join(root, "fabricated.md")
	plain := filepath.Join(root, "ignored.txt")
	if err := os.WriteFile(markdown, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plain, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := newPolling(root, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })

	if err := os.WriteFile(markdown, []byte("second version\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plain, []byte("second version\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-source.Events():
		if got != markdown {
			t.Fatalf("event path = %q, want %q", got, markdown)
		}
	case err := <-source.Errors():
		t.Fatalf("polling error: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("polling fallback did not emit the Markdown change")
	}
}
