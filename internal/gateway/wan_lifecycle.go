package gateway

import (
	"context"
	"errors"
	"sync"

	"github.com/yetone/magpie/internal/wan"
)

// wanRuntime belongs to the server that acquired the gateway's listener.
// Merely constructing a Server or opening its page publishes nothing.
type wanRuntime struct {
	mu      sync.Mutex
	ctx     context.Context
	manager *wan.Manager
}

func (s *Server) ownWAN(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	manager := wan.New(s.WANHandler())
	if err := manager.Own(ctx); err != nil {
		cancel()
	}
	s.wan.mu.Lock()
	s.wan.ctx, s.wan.manager = ctx, manager
	s.wan.mu.Unlock()
	return func() {
		cancel()
		manager.Close()
		s.wan.mu.Lock()
		s.wan.ctx = nil
		s.wan.mu.Unlock()
	}
}

func (s *Server) WANStatus() wan.Status {
	s.wan.mu.Lock()
	manager := s.wan.manager
	s.wan.mu.Unlock()
	if manager == nil {
		return wan.Status{State: "stopped"}
	}
	return manager.Status()
}

// StartWAN uses the gateway's lifetime, not the browser request's: leaving
// the page must not shut the tunnel down.
func (s *Server) StartWAN() error {
	s.wan.mu.Lock()
	manager, ctx := s.wan.manager, s.wan.ctx
	s.wan.mu.Unlock()
	if manager == nil || ctx == nil || ctx.Err() != nil {
		return errors.New("This process is not serving the gateway")
	}
	return manager.Start(ctx)
}

func (s *Server) wanManager() (*wan.Manager, context.Context, error) {
	s.wan.mu.Lock()
	manager, ctx := s.wan.manager, s.wan.ctx
	s.wan.mu.Unlock()
	if manager == nil || ctx == nil || ctx.Err() != nil {
		return nil, nil, errors.New("This process is not serving the gateway")
	}
	return manager, ctx, nil
}

func (s *Server) StartWANAuth(remember bool, provider string) (wan.AuthStatus, error) {
	manager, _, err := s.wanManager()
	if err != nil {
		return wan.AuthStatus{}, err
	}
	return manager.StartAuth(remember, provider)
}

func (s *Server) CancelWANAuth(attemptID uint64) error {
	manager, _, err := s.wanManager()
	if err != nil {
		return err
	}
	if !manager.CancelAuth(attemptID) {
		return errors.New("WAN sign-in attempt is no longer current")
	}
	return nil
}

func (s *Server) LogoutWAN() error {
	manager, _, err := s.wanManager()
	if err != nil {
		return err
	}
	return manager.Logout()
}

func (s *Server) InstallWANRuntime() error {
	manager, ctx, err := s.wanManager()
	if err != nil {
		return err
	}
	return manager.InstallRuntime(ctx)
}

func (s *Server) StopWAN() error {
	s.wan.mu.Lock()
	manager := s.wan.manager
	s.wan.mu.Unlock()
	if manager != nil {
		manager.Stop()
	}
	return nil
}

func (s *Server) RefreshWANUsage() error {
	m, _, e := s.wanManager()
	if e != nil {
		return e
	}
	return m.RefreshUsage()
}
func (s *Server) NewWANTunnel() error {
	m, _, e := s.wanManager()
	if e != nil {
		return e
	}
	return m.RequestNewTunnel()
}
