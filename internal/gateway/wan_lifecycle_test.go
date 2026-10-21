package gateway

import (
	"context"
	"testing"
)

func TestWANRequiresServingOwner(t *testing.T) {
	s := New()
	if err := s.StartWAN(); err == nil {
		t.Fatal("an unserved gateway accepted WAN startup")
	}
	if got := s.WANStatus(); got.State != "stopped" || got.Endpoint != "" {
		t.Fatal("constructing a gateway published a WAN endpoint")
	}
	if err := s.StopWAN(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartWANAuth(false); err == nil {
		t.Fatal("an unserved gateway started authorization")
	}
	if err := s.CancelWANAuth(1); err == nil {
		t.Fatal("an unserved gateway accepted authorization cancellation")
	}
	if err := s.LogoutWAN(); err == nil {
		t.Fatal("an unserved gateway accepted credential deletion")
	}
	if err := s.InstallWANRuntime(); err == nil {
		t.Fatal("an unserved gateway accepted a runtime installation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop := s.ownWAN(ctx)
	t.Cleanup(stop)
	if got := s.WANStatus(); got.State != "stopped" || got.Endpoint != "" {
		t.Fatal("acquiring ownership started WAN without the user's action")
	}
	cancel()
	if err := s.StartWAN(); err == nil {
		t.Fatal("a canceled owner accepted WAN startup")
	}
	stop()
	if err := s.StartWAN(); err == nil {
		t.Fatal("a finished owner accepted WAN startup")
	}
}
