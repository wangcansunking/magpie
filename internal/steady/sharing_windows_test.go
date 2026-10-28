package steady

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Our reader coordination never overrides another process's handle. A
// persistent denial still returns the write error, keeping the old file.
func TestWriteHeldFileKeepsOldGeneration(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "plugins.json")
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	name, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.CloseHandle(h) })
	if err := WriteFile(p, []byte("new"), 0o600); err == nil {
		t.Fatal("reported a write over an unshared handle as successful")
	}
	if b, err := ReadFile(p); err != nil || string(b) != "old" {
		t.Fatalf("failed write lost the old generation: %q, %v", b, err)
	}
	if files, err := os.ReadDir(dir); err != nil || len(files) != 1 || files[0].Name() != "plugins.json" {
		t.Fatalf("failed write left temporary files: %v, %v", files, err)
	}
}
