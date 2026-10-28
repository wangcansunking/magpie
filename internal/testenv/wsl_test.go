package testenv

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
)

// WSL reads another OS's home, outside the Windows test's redirected HOME.
// Only our own copied binary is ever reachable as an installed wsl.exe,
// including when the barrier is broken to prove the fixture catches it.
func TestWSLStandInBlocksInstalledExecutable(t *testing.T) {
	installed, home := t.TempDir(), t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(installed, "wsl.exe")
	if err := os.WriteFile(sentinel, b, 0o755); err != nil {
		t.Fatal(err)
	}
	// Never retain the real PATH, even for the negative control below.
	t.Setenv("PATH", installed)
	t.Setenv("NoDefaultCurrentDirectoryInExePath", "1")
	if err := inertTools(home); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "bin", "wsl.exe")
	if got, err := exec.LookPath("wsl.exe"); err != nil || got != want {
		t.Errorf("wsl.exe resolved to %q (%v), want inert file %q", got, err, want)
	}
	for _, bounded := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var cmd *exec.Cmd
		if bounded {
			cmd = proc.CommandContext(ctx, "wsl.exe", "--magpie-test-wsl-sentinel")
		} else {
			cmd = proc.Command("wsl.exe", "--magpie-test-wsl-sentinel")
		}
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil || len(out) != 0 {
			t.Errorf("bounded=%v: installed WSL sentinel reached through barrier: output=%q err=%v", bounded, out, err)
		}
	}
	// Prove the later PATH entry really runs, without a host path present.
	if err := os.Remove(want); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Remove(want + ".bat"); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if got, err := exec.LookPath("wsl.exe"); err != nil || got != sentinel {
		t.Fatalf("negative control resolved %q (%v), want our sentinel %q", got, err, sentinel)
	}
	// This checks executable selection, not startup latency. A copied test
	// binary can spend more than ten seconds in Windows' executable scanner.
	// Let the test own the child rather than racing that unrelated startup.
	out, err := proc.CommandContext(t.Context(), "wsl.exe", "--magpie-test-wsl-sentinel").CombinedOutput()
	if err != nil || string(out) != "test WSL sentinel reached\n" {
		t.Fatalf("negative control did not reach our sentinel: %q %v", out, err)
	}
}
