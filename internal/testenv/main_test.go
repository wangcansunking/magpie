package testenv

import (
	"os"
	"testing"
)

// This package's tests run without the machine's proxy variables too:
// Offline's guard sits below the proxy hop, so one standing on loopback
// carries a refused host for real, and TestOffline then answers
// models.dev through it.
func TestMain(m *testing.M) {
	// A copy of this binary is the installed WSL sentinel, never the host's
	// wsl.exe. It exits before any other test setup can run.
	if len(os.Args) == 2 && os.Args[1] == "--magpie-test-wsl-sentinel" {
		os.Stdout.WriteString("test WSL sentinel reached\n")
		os.Exit(0)
	}
	ClearProxy()
	os.Exit(m.Run())
}
