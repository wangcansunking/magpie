package gui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/wan"
)

func TestWANRecoveryAlertOncePerFailure(t *testing.T) {
	var a wanRecoveryAlert
	failure := wan.Status{State: "failed", Error: wan.RelayRecoveryTimeoutMessage}
	for _, step := range []struct {
		status wan.Status
		want   bool
	}{
		{wan.Status{State: "running"}, false}, {failure, true}, {failure, false},
		{wan.Status{State: "stopped"}, false}, {failure, true},
		{wan.Status{State: "failed", Error: "sign-in required"}, false},
	} {
		if got := a.observe(step.status); got != step.want {
			t.Fatalf("notify(%q) = %v, want %v", step.status.State, got, step.want)
		}
	}
}

func TestWANRecoveryAlertWorkerEndsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	called := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchWANRecovery(ctx, ticks, func() wan.Status { return wan.Status{State: "failed", Error: wan.RelayRecoveryTimeoutMessage} }, func() { called <- struct{}{} })
	}()
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("recovery failure did not notify")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("notification worker outlived cancellation")
	}
	select {
	case <-called:
		t.Fatal("duplicate failure notification")
	default:
	}
}

func TestWANRecoveryAlertTextAllLanguages(t *testing.T) {
	for _, lang := range []string{"en", "zh", "zh-TW", "ja", "de"} {
		title, body := wanRecoveryAlertText(lang)
		if title == "" || !strings.Contains(body, "20") || !strings.Contains(body, "5") {
			t.Fatalf("missing cadence/deadline in %s alert", lang)
		}
	}
}
