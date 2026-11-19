//go:build dev

package wan

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// Development builds read the rebuilt helper on each explicit Start, just as
// the GUI reads source assets. Editing it does not discard an in-memory login.
// Release builds always use the embedded bundle.
func sdkHelper() ([]byte, error) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return nil, errors.New("WAN helper source is unavailable")
	}
	f, err := os.Open(filepath.Join(filepath.Dir(source), "sdk", "host.cjs"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const limit = 4 << 20
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > limit {
		return nil, errors.New("WAN helper source has an invalid size")
	}
	return data, nil
}
