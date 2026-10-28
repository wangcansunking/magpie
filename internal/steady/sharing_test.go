package steady

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The reader stays inside ReadFile until the replacement has queued. A
// rename without reader coordination exhausts its retries on Windows;
// it must instead wait for the reader to close, then expose the new bytes.
func TestReplacementWaitsForOurReader(t *testing.T) {
	wasWindows, wasRead := windows, readFile
	windows = true
	t.Cleanup(func() { windows, readFile = wasWindows, wasRead })
	for _, read := range []struct {
		name string
		fn   func(string) ([]byte, error)
	}{{"ReadFile", ReadFile}, {"ReadThere", ReadThere}} {
		t.Run(read.name, func(t *testing.T) {
			dir := t.TempDir()
			p, next := filepath.Join(dir, "plugins.json"), filepath.Join(dir, "next.json")
			for name, body := range map[string]string{p: "old", next: "new"} {
				if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			letGo := sync.OnceFunc(func() { close(release) })
			readFile = func(name string) ([]byte, error) {
				f, err := os.Open(name)
				if err != nil {
					return nil, err
				}
				defer f.Close()
				close(entered)
				<-release
				b := make([]byte, 3)
				_, err = f.Read(b)
				return b, err
			}
			readDone := make(chan struct{})
			go func() {
				defer close(readDone)
				if b, err := read.fn(p); err != nil || string(b) != "old" {
					t.Errorf("reader lost its generation: %q, %v", b, err)
				}
			}()
			t.Cleanup(func() { letGo(); <-readDone; readFile = wasRead })
			select {
			case <-entered:
			case <-readDone:
				t.Fatal("reader exited before opening its file")
			case <-time.After(5 * time.Second):
				t.Fatal("reader never opened its file")
			}
			written := make(chan error, 1)
			writerDone := make(chan struct{})
			go func() { defer close(writerDone); written <- Rename(next, p) }()
			t.Cleanup(func() { letGo(); <-writerDone })
			key := strings.ToLower(filepath.Clean(p))
			limit := time.NewTimer(5 * time.Second)
			defer limit.Stop()
			for {
				sharing.Lock()
				r := sharing.paths[key]
				queued := r != nil && r.users == 2
				sharing.Unlock()
				if queued {
					break
				}
				select {
				case err := <-written:
					t.Fatalf("replacement did not wait for our reader: %v", err)
				case <-limit.C:
					t.Fatal("replacement never queued behind its reader")
				default:
					runtime.Gosched()
				}
			}
			letGo()
			<-readDone
			if err := <-written; err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(p); err != nil || string(b) != "new" {
				t.Fatalf("replacement not visible: %q, %v", b, err)
			}
			sharing.Lock()
			left := len(sharing.paths)
			sharing.Unlock()
			if left != 0 {
				t.Fatalf("kept %d idle path locks", left)
			}
		})
	}
}

func TestReaderCoordinationIsPerPath(t *testing.T) {
	was := windows
	windows = true
	t.Cleanup(func() { windows = was })
	dir := t.TempDir()
	one := filepath.Join(dir, "one")
	leave := shareRead(one)
	defer leave()
	// Reading one file must not stop another file's writer.
	other, done := readersFor(filepath.Join(dir, "two"))
	defer done()
	if !other.TryLock() {
		t.Fatal("a reader blocked another file's replacement")
	}
	other.Unlock()
	t.Chdir(dir)
	// Join would clean the dots before readersFor sees them.
	for _, name := range []string{dir + string(filepath.Separator) + "nested" + string(filepath.Separator) + ".." + string(filepath.Separator) + "one", "one", strings.ToUpper(one)} {
		alias, done := readersFor(name)
		if alias.TryLock() {
			alias.Unlock()
			done()
			t.Fatalf("alias %q escaped its reader", name)
		}
		done()
	}
}
