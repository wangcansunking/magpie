package plugin

import (
	"context"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Bun checks TLS against its own roots: behind a proxy that re-signs HTTPS
// with a root the user installed, `bun add` failed with
// UNABLE_TO_VERIFY_LEAF_SIGNATURE (chenyyi001 on X, installing
// @magpie-community/opencode-kiro-auth). Every bun magpie runs is given
// the system's roots, unless the user names a file of their own.
func TestBunIsGivenTheSystemsRoots(t *testing.T) {
	old := systemCAFile
	t.Cleanup(func() { systemCAFile = old })
	systemCAFile = func() string { return "/cache/bun/system-ca-abc.pem" }
	// one the environment sets already is the user's and wins (below);
	// the ambient one a shell may have isn't this test's
	t.Setenv("NODE_EXTRA_CA_CERTS", "")

	cmd := bunCommand(context.Background(), "bun", t.TempDir(), "add", "x")
	if !slices.Contains(cmd.Env, "NODE_EXTRA_CA_CERTS=/cache/bun/system-ca-abc.pem") {
		t.Fatalf("bun add isn't given the system's roots: %v", caVars(cmd.Env))
	}

	t.Setenv("NODE_EXTRA_CA_CERTS", "/mine.pem")
	cmd = bunCommand(context.Background(), "bun", t.TempDir(), "add", "x")
	if got := caVars(cmd.Env); !slices.Equal(got, []string{"NODE_EXTRA_CA_CERTS=/mine.pem"}) {
		t.Fatalf("the user's own NODE_EXTRA_CA_CERTS = %v, want it alone", got)
	}

	systemCAFile = func() string { return "" }
	if got := caEnv(nil, ""); !slices.Equal(got, []string{"NODE_USE_SYSTEM_CA=1"}) {
		t.Fatalf("with no file to add: %v, want Bun to read the system's store itself", got)
	}
}

func TestBunCAEnvForIndependentHelper(t *testing.T) {
	old := systemCAFile
	t.Cleanup(func() { systemCAFile = old })
	systemCAFile = func() string { return "/cache/bun/system-ca.pem" }
	got, err := BunCAEnv([]string{"NODE_OPTIONS=--require=untrusted", "BUN_OPTIONS=--preload=untrusted", "HTTPS_PROXY=http://untrusted"}, t.TempDir())
	if err != nil || !slices.Equal(got, []string{"NODE_USE_SYSTEM_CA=1", "NODE_EXTRA_CA_CERTS=/cache/bun/system-ca.pem"}) {
		t.Fatalf("system roots for helper: %v %v", got, err)
	}
	path, err := filepath.Abs("my-relative-ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	got, err = BunCAEnv([]string{"NODE_EXTRA_CA_CERTS=my-relative-ca.pem"}, dir)
	want := "NODE_EXTRA_CA_CERTS=" + bunReadable(path, dir, runtime.GOOS)
	if err != nil || !slices.Equal(got, []string{"NODE_USE_SYSTEM_CA=1", want}) {
		t.Fatalf("user CA must resolve before helper cwd changes: %v %v want %q", got, err, want)
	}
	for _, system := range []string{"0", "1"} {
		got, err = BunCAEnv([]string{"node_use_system_ca=" + system, "NODE_EXTRA_CA_CERTS=my-relative-ca.pem", "NODE_OPTIONS=--require=untrusted"}, dir)
		if err != nil || !slices.Equal(got, []string{"NODE_USE_SYSTEM_CA=" + system, want}) {
			t.Fatalf("helper changed user's system trust override: %v %v", got, err)
		}
	}
	systemCAFile = func() string { return "" }
	got, err = BunCAEnv(nil, dir)
	if err != nil || !slices.Equal(got, []string{"NODE_USE_SYSTEM_CA=1"}) {
		t.Fatalf("helper without a bundle lost system trust: %v %v", got, err)
	}
	if runtime.GOOS == "windows" {
		ca := `C:\Users\张三\ca.pem`
		dir := `C:\Users\张三\AppData\Local\Temp\wan-helper`
		got, err = BunCAEnv([]string{"NODE_EXTRA_CA_CERTS=" + ca}, dir)
		if err != nil || len(got) != 2 || got[0] != "NODE_USE_SYSTEM_CA=1" || got[1] != "NODE_EXTRA_CA_CERTS="+bunReadable(ca, dir, "windows") || !ascii(strings.TrimPrefix(got[1], "NODE_EXTRA_CA_CERTS=")) {
			t.Fatalf("non-ASCII user CA not readable by Bun: %v %v", got, err)
		}
	}
}

// Bun reads the system's store itself (NODE_USE_SYSTEM_CA): magpie's copy
// of the ROOT store missed what Bun's own reading has (the CA store's
// intermediates a proxy doesn't send, the enterprise's and group
// policy's roots), and a plugin answered "self signed certificate in
// certificate chain" while bun add failed with
// UNABLE_TO_GET_ISSUER_CERT_LOCALLY (明霞 on Discord). A value the user
// gives it, 0 too, is theirs.
func TestBunReadsTheSystemsStore(t *testing.T) {
	old := systemCAFile
	t.Cleanup(func() { systemCAFile = old })
	systemCAFile = func() string { return "/cache/bun/system-ca-abc.pem" }
	t.Setenv("NODE_EXTRA_CA_CERTS", "")
	t.Setenv("NODE_USE_SYSTEM_CA", "")

	cmd := bunCommand(context.Background(), "bun", t.TempDir(), "run", "host.js")
	if got := envVars(cmd.Env, "NODE_USE_SYSTEM_CA"); !slices.Equal(got, []string{"NODE_USE_SYSTEM_CA=1"}) {
		t.Fatalf("Bun isn't told to read the system's store: %v", got)
	}

	// the user's own file of roots is taken beside the system's store
	t.Setenv("NODE_EXTRA_CA_CERTS", "/mine.pem")
	cmd = bunCommand(context.Background(), "bun", t.TempDir(), "add", "x")
	if got := envVars(cmd.Env, "NODE_USE_SYSTEM_CA"); !slices.Equal(got, []string{"NODE_USE_SYSTEM_CA=1"}) {
		t.Fatalf("with the user's NODE_EXTRA_CA_CERTS: %v", got)
	}

	t.Setenv("NODE_USE_SYSTEM_CA", "0")
	cmd = bunCommand(context.Background(), "bun", t.TempDir(), "add", "x")
	if got := envVars(cmd.Env, "NODE_USE_SYSTEM_CA"); !slices.Equal(got, []string{"NODE_USE_SYSTEM_CA=0"}) {
		t.Fatalf("the user's own NODE_USE_SYSTEM_CA = %v, want it alone", got)
	}
}

func envVars(env []string, name string) []string {
	var out []string
	for _, kv := range env {
		// a blank one (t.Setenv's "") is no value; exec takes the last
		if strings.HasPrefix(strings.ToUpper(kv), name+"=") && kv != name+"=" {
			out = append(out, kv)
		}
	}
	return out
}

func caVars(env []string) []string {
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(strings.ToUpper(kv), "NODE_EXTRA_CA_CERTS=") {
			out = append(out, kv)
		}
	}
	return out
}

// Bun on Windows can't open a NODE_EXTRA_CA_CERTS path that isn't ASCII
// (#1232: a Chinese user name), so there it is given the way to the file
// from where it runs, which is under the same user folder.
func TestBunIsGivenAnASCIIWayToTheRoots(t *testing.T) {
	user := `C:\Users\张三`
	ca := user + `\.cache\magpie\bun\system-ca-abc.pem`
	for _, c := range []struct{ name, p, dir, goos, want string }{
		{"under a Chinese user name", ca, user + `\.config\magpie\plugins`, "windows", `..\..\..\.cache\magpie\bun\system-ca-abc.pem`},
		{"an ASCII path as it is", `C:\Users\bob\.cache\magpie\bun\system-ca-abc.pem`, `C:\Users\bob\x`, "windows", `C:\Users\bob\.cache\magpie\bun\system-ca-abc.pem`},
		{"another drive, no way there", ca, `D:\plugins`, "windows", ca},
		{"no way there in ASCII", ca, `C:\其他\plugins`, "windows", ca},
		{"Bun elsewhere reads it whole", "/home/张三/.cache/magpie/bun/system-ca-abc.pem", "/home/张三/.config/magpie/plugins", "linux", "/home/张三/.cache/magpie/bun/system-ca-abc.pem"},
	} {
		if c.goos == "windows" && runtime.GOOS != "windows" {
			// filepath.Rel works on this OS's paths; Windows' are tried there
			continue
		}
		if got := bunReadable(c.p, c.dir, c.goos); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if !ascii(`..\..\x.pem`) || ascii(`C:\Users\张三`) {
		t.Fatal("ascii")
	}
}
