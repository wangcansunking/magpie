package steady

import (
	"path/filepath"
	"strings"
	"sync"
)

// On Windows our readers leave a gap for a pending replacement. Retrying
// the rename alone can exhaust its wait while overlapping readers keep
// the destination open continuously. Other processes still need retry.
var sharing struct {
	sync.Mutex
	paths map[string]*fileReaders
}

type fileReaders struct {
	sync.RWMutex
	users int
}

// readersFor holds an entry until done, including time waiting for its
// lock. Idle paths are dropped so a long-running gateway keeps no history
// of every account, session or temporary file it has read.
func readersFor(p string) (*fileReaders, func()) {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	p = strings.ToLower(filepath.Clean(p))
	sharing.Lock()
	if sharing.paths == nil {
		sharing.paths = make(map[string]*fileReaders)
	}
	r := sharing.paths[p]
	if r == nil {
		r = &fileReaders{}
		sharing.paths[p] = r
	}
	r.users++
	sharing.Unlock()
	return r, func() {
		sharing.Lock()
		if r.users--; r.users == 0 {
			delete(sharing.paths, p)
		}
		sharing.Unlock()
	}
}

func shareRead(p string) func() {
	if !windows {
		return func() {}
	}
	r, done := readersFor(p)
	r.RLock()
	return func() { r.RUnlock(); done() }
}

func shareReplace(p string) func() {
	if !windows {
		return func() {}
	}
	r, done := readersFor(p)
	r.Lock()
	return func() { r.Unlock(); done() }
}
