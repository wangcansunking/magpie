package wan

import "sync"

// processTunnels tracks idle owners as well as active hosting. A device grant,
// credential restore or explicit runtime install must not outlive process exit.
var processTunnels = tunnelRuntime{}

type tunnelRuntime struct {
	mu       sync.Mutex
	active   map[*hosting]struct{}
	owners   map[*Manager]struct{}
	shutdown chan struct{}
}

// Shutdown permanently closes WAN's start/auth/install gates and joins all
// owners. Stop remains the restartable control for a single hosting generation.
func Shutdown() { processTunnels.stop() }

func (rt *tunnelRuntime) stop() {
	rt.mu.Lock()
	if rt.shutdown != nil {
		done := rt.shutdown
		rt.mu.Unlock()
		<-done
		return
	}
	rt.shutdown = make(chan struct{})
	done := rt.shutdown
	active := make([]*hosting, 0, len(rt.active))
	for r := range rt.active {
		active = append(active, r)
	}
	owners := make([]*Manager, 0, len(rt.owners))
	for m := range rt.owners {
		owners = append(owners, m)
	}
	rt.mu.Unlock()
	// Revoke every listener before joining any cleanup or vault operation.
	for _, r := range active {
		r.revoke()
		r.cancel()
	}
	for _, m := range owners {
		m.mu.Lock()
		m.closed = true
		m.cancel()
		m.mu.Unlock()
	}
	for _, r := range active {
		<-r.done
	}
	for _, m := range owners {
		<-m.ownerDone
	}
	close(done)
}
