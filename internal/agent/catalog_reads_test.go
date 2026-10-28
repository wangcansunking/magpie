package agent

import (
	"testing"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
)

// Own model names, empty fields and effort values cannot be magpie refs,
// even when Resolve could find a provider serving that bare model name.
func TestOwnValuesBuildNoCatalog(t *testing.T) {
	codexHome(t, "", "")
	plugin.UseCached(nil)
	n := provider.AllBuilt.Load()
	for _, v := range []string{"", "m1", "opus", "high", "magpie"} {
		if isMagpie(v) {
			t.Errorf("own value %q taken for a catalog ref", v)
		}
	}
	if got := provider.AllBuilt.Load() - n; got != 0 {
		t.Fatalf("own values built the providers %d times, want none", got)
	}
	if !isMagpie("fake/m1") || !isMagpie("fake/m1[1m]") || isMagpie("absent/m1") {
		t.Fatal("catalog references no longer resolve as before")
	}
}
