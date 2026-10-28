package sqlitedb

import (
	"context"
	"database/sql"
	"encoding/binary"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"modernc.org/libc"
)

// childDB names the database the started-again test binary opens; only
// that child runs TestOpenInChild.
const childDB = "MAGPIE_SQLITEDB_CHILD"

// The environment #2327's reporter's magpie was started with: a fragment
// of a DLP tool's dylib path, with no "=", ahead of everything else. The
// first SQLite open reads SQLITE_TMPDIR, and modernc's getenv used to
// print TODOTODO and os.Exit(1) on that entry (on macOS, Windows and the
// BSDs; Linux's musl-based getenv skips it). Now the open works.
func TestSQLiteSurvivesMalformedEnv(t *testing.T) {
	db := filepath.Join(t.TempDir(), "x.db")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOpenInChild$", "-test.count=1", "-test.v")
	child.Env = os.Environ()
	if runtime.GOOS != "windows" {
		child.Env = append([]string{"hains/MacSec/libesafenet.dylib"}, child.Env...)
	}
	child.Env = append(child.Env, childDB+"="+db)
	out, err := child.CombinedOutput()
	if err != nil || strings.Contains(string(out), "TODOTODO") || !strings.Contains(string(out), "opened 1") {
		t.Fatalf("a magpie started with an entry with no \"=\" couldn't open SQLite: %v\n%s", err, out)
	}
}

// TestOpenInChild is the started-again binary's half of
// TestSQLiteSurvivesMalformedEnv: it opens a database and reads from it.
func TestOpenInChild(t *testing.T) {
	path := os.Getenv(childDB)
	if path == "" {
		t.Skip("run by TestSQLiteSurvivesMalformedEnv")
	}
	if runtime.GOOS == "windows" {
		// CreateProcess rejects an entry lacking "=", and SetEnviron is
		// one-shot (our init already called it). Replace libc's copy only
		// in this child. Windows SQLite uses Win32's temp path, so exercise
		// libc's actual getenv too, not just an unrelated successful open.
		env := wellFormed([]string{"hains/MacSec/libesafenet.dylib", "SQLITE_TMPDIR=fixture"})
		// CString copies arbitrary bytes, including NULs, into libc-owned
		// memory. Encode its pointer table there too: getenv does uintptr
		// arithmetic, which must not point into a Go allocation.
		var entries []byte
		for _, kv := range env {
			p, err := libc.CString(kv)
			if err != nil {
				t.Fatal(err)
			}
			if strconv.IntSize == 64 {
				entries = binary.NativeEndian.AppendUint64(entries, uint64(p))
			} else {
				entries = binary.NativeEndian.AppendUint32(entries, uint32(p))
			}
			defer libc.Xfree(nil, p)
		}
		entries = append(entries, make([]byte, strconv.IntSize/8)...)
		table, err := libc.CString(string(entries))
		if err != nil {
			t.Fatal(err)
		}
		defer libc.Xfree(nil, table)
		before := libc.Xenviron
		libc.Xenviron = table
		defer func() { libc.Xenviron = before }()
		name, err := libc.CString("SQLITE_TMPDIR")
		if err != nil {
			t.Fatal(err)
		}
		defer libc.Xfree(nil, name)
		if got := libc.GoString(libc.Xgetenv(nil, name)); got != "fixture" {
			t.Fatalf("libc getenv after filtering: %q", got)
		}
	} else if !slices.Contains(os.Environ(), "hains/MacSec/libesafenet.dylib") {
		t.Fatal("the child wasn't handed the entry with no \"=\"")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT 1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	t.Log("opened " + strconv.Itoa(n))
}

func TestWellFormed(t *testing.T) {
	in := []string{"PATH=/bin", "hains/MacSec/libesafenet.dylib", "", "=C:=C:\\x", "EMPTY=", "A=b=c"}
	want := []string{"PATH=/bin", "=C:=C:\\x", "EMPTY=", "A=b=c"}
	if got := wellFormed(in); !slices.Equal(got, want) {
		t.Fatalf("wellFormed(%q) = %q, want %q", in, got, want)
	}
}

// Every SQLite open goes through this package, so libc's environment is
// cleaned before any of them: no other package's own code imports
// modernc's driver or its libc. A test file may, to build its fixtures.
func TestOnlyThisPackageReachesSQLite(t *testing.T) {
	root, _ := filepath.Abs(filepath.Join("..", ".."))
	self := filepath.Join(root, "internal", "sqlitedb")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" || d.Name() == "testdata" || path == self) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, im := range f.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if p == "modernc.org/sqlite" || strings.HasPrefix(p, "modernc.org/sqlite/") || p == "modernc.org/libc" {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s imports %s; import internal/sqlitedb instead, which cleans libc's environment first (#2327)", rel, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
