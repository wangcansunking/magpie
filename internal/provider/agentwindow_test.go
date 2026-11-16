package provider

import (
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/settings"
)

func TestDesktopMaximumContexts(t *testing.T) {
	isolate(t)
	limitsHome(t)
	models := []catalog.Model{
		{ID: "sol", Context: 272000, MaxContext: 922000},
		{ID: "terra", Context: 272000, MaxContext: 1000000},
		{ID: "astra", Context: 272000, MaxContext: 1050000},
		{ID: "luna", Context: 272000, MaxContext: 872000},
	}
	p := Provider{ID: "relay", Name: "Relay", Key: "k", Chat: "http://127.0.0.1:1/v1", Models: []string{"sol", "terra", "astra", "luna"}}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive(p.ID, p.Chat, models); err != nil {
		t.Fatal(err)
	}
	s := settings.Load()
	s.DesktopLongest = true
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	check := func(agent string, wantMax bool) {
		t.Helper()
		entries, _ := CatalogFor(agent)
		for _, m := range models {
			found := false
			for _, e := range entries {
				if e.ID != "relay/"+m.ID {
					continue
				}
				found = true
				want := m.Context
				if wantMax {
					want = m.MaxContext
				}
				if e.Context != want {
					t.Errorf("%s %s: %d, want %d", agent, m.ID, e.Context, want)
				}
			}
			if !found {
				t.Errorf("missing %s", m.ID)
			}
		}
	}
	check("claude-desktop", true)
	check("claude", false)
	s.DesktopLongest = false
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	check("claude-desktop", false)
	s.DesktopLongest = true
	s.ContextCap = 500000
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	for _, e := range CatalogOf("claude-desktop") {
		if e.Provider.ID == p.ID && e.Context != 500000 {
			t.Errorf("global cap lost: %s %d", e.ID, e.Context)
		}
	}
	s.ContextCap = 0
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	if err := SetContext(p, "sol", 700000); err != nil {
		t.Fatal(err)
	}
	for _, e := range CatalogOf("claude-desktop") {
		if e.ID == "relay/sol" && e.Context != 700000 {
			t.Errorf("provider override lost: %d", e.Context)
		}
	}
	if err := SetAgentContexts("claude-desktop", map[string]int{"relay/sol": 650000}); err != nil {
		t.Fatal(err)
	}
	shown, _ := CatalogFor("claude-desktop")
	for _, e := range shown {
		if e.ID == "relay/sol" && e.Context != 650000 {
			t.Errorf("agent override lost: %d", e.Context)
		}
	}
}

// The window an agent is told for a model can be its own (ul3140773077392
// on X: dsh compacts earlier than the other agents at the same window).
// An agent's entry comes before the provider's own window, is a window the
// user typed (so Codex compacts at all of it), leaves every other agent
// alone, "*" only shortens a window that is known, and taking it away
// gives the model's own again.
func TestAgentOwnContextWindow(t *testing.T) {
	isolate(t)
	limitsHome(t)
	if err := Save(Provider{ID: "relay", Name: "Relay", Key: "k", Chat: "http://127.0.0.1:1/v1", Models: []string{"m1", "m2"}, Contexts: map[string]int{"m1": 200000, "m2": 100000}}); err != nil {
		t.Fatal(err)
	}
	if err := SetAutoGroups(false); err != nil {
		t.Fatal(err)
	}
	if err := SaveGroup(Group{ID: "mix", Name: "mix", Members: []string{"relay/m1", "relay/m2"}}); err != nil {
		t.Fatal(err)
	}
	window := func(agent, id string) (int, bool) {
		shown, _ := CatalogFor(agent)
		for _, e := range shown {
			if e.ID == id {
				return e.Context, e.WindowSet
			}
		}
		t.Fatalf("%s: no %s in %v", agent, id, ids(shown))
		return 0, false
	}
	if n, set := window("dsh", "relay/m1"); n != 200000 || !set {
		t.Fatalf("to begin with: %d %v", n, set)
	}
	if err := SetAgentContexts("dsh", map[string]int{"relay/m1": 500000, "*": 150000}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		agent, id string
		n         int
		set       bool
	}{
		{"dsh", "relay/m1", 500000, true},   // its own entry, even past the model's
		{"dsh", "relay/m2", 100000, true},   // "*" only shortens: m2's 100K is shorter than 150K
		{"codex", "relay/m1", 200000, true}, // the others keep what the provider says
		{"pi", "relay/m2", 100000, true},
	} {
		if n, set := window(c.agent, c.id); n != c.n || set != c.set {
			t.Errorf("%s %s: %d %v; want %d %v", c.agent, c.id, n, set, c.n, c.set)
		}
	}
	// "*" shortens a window that is longer than it
	if err := SetAgentContexts("dsh", map[string]int{"*": 150000}); err != nil {
		t.Fatal(err)
	}
	if n, set := window("dsh", "relay/m1"); n != 150000 || !set {
		t.Errorf("* on a longer window: %d %v", n, set)
	}
	// a provider's, and a group's own entry
	if err := SetAgentContexts("dsh", map[string]int{"relay/*": 64000, "group/mix": 90000}); err != nil {
		t.Fatal(err)
	}
	if n, _ := window("dsh", "relay/m2"); n != 64000 {
		t.Errorf("provider/*: %d", n)
	}
	if n, set := window("dsh", "group/mix"); n != 90000 || !set {
		t.Errorf("the group: %d %v", n, set)
	}
	// the window typed is what Codex-style readers compact at
	shown, _ := CatalogFor("dsh")
	s := settings.Load()
	for _, e := range shown {
		if e.ID == "relay/m1" {
			if got := CompactFor(s, e, GroupFinder()); got != 64000 {
				t.Errorf("compacts at %d; want the whole window, 64000", got)
			}
		}
	}
	if err := SetAgentContexts("dsh", map[string]int{"relay/m1": -1}); err == nil {
		t.Error("a negative window was accepted")
	}
	if err := SetAgentContexts("dsh", nil); err != nil {
		t.Fatal(err)
	}
	if n, set := window("dsh", "relay/m1"); n != 200000 || !set {
		t.Errorf("taken away: %d %v", n, set)
	}
	if got := settings.Load().AgentContexts; len(got) != 0 {
		t.Errorf("kept %v", got)
	}
}

// A routing group carries its own compaction size (NovaStudiosss on X): it
// is where agents compact on the group, before the smallest its members
// say, and with none set nothing changes.
func TestGroupOwnCompactAt(t *testing.T) {
	isolate(t)
	limitsHome(t)
	if err := Save(Provider{ID: "relay", Name: "Relay", Key: "k", Chat: "http://127.0.0.1:1/v1", Models: []string{"m1", "m2"}}); err != nil {
		t.Fatal(err)
	}
	if err := SetModelCompacts("relay", map[string]int{"m1": 250000, "m2": 300000}); err != nil {
		t.Fatal(err)
	}
	if err := SetAutoGroups(false); err != nil {
		t.Fatal(err)
	}
	if err := SaveGroup(Group{ID: "mix", Name: "mix", Members: []string{"relay/m1", "relay/m2"}}); err != nil {
		t.Fatal(err)
	}
	if got := CompactSet("group/mix"); got != 250000 {
		t.Errorf("none of its own: %d; want its smallest member's", got)
	}
	if err := SaveGroup(Group{ID: "mix", Name: "mix", Members: []string{"relay/m1", "relay/m2"}, Compact: 160000}); err != nil {
		t.Fatal(err)
	}
	if got := CompactSet("group/mix"); got != 160000 {
		t.Errorf("its own: %d", got)
	}
	if err := SetModelCompacts("relay", nil); err != nil {
		t.Fatal(err)
	}
	if got := CompactSet("group/mix"); got != 160000 {
		t.Errorf("its own with no model set: %d", got)
	}
	if err := SaveGroup(Group{ID: "mix", Name: "mix", Members: []string{"relay/m1"}, Compact: -5}); err != nil {
		t.Fatal(err)
	}
	if g, _, ok := FindGroup(GroupPrefix + "mix"); !ok || g.Compact != 0 {
		t.Errorf("a negative size was kept: %+v", g)
	}
}
