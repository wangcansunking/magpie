package proc

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// A short-lived real command exercises cmd.Wait on Windows too. The group
// wrapper holds only cleanup completion, not a process or the tree's mutex.
func TestTreeCommandExit(t *testing.T) {}

func completionTree(t *testing.T) *Tree {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tree, err := StartTree(Command(self, "-test.run=^TestTreeCommandExit$"))
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

type heldTreeGroup struct {
	treeGroup
	killDone, sweepDone <-chan struct{}
	termErr             error
	kills, sweeps       int
}

func (g *heldTreeGroup) term(*exec.Cmd) error { return g.termErr }
func (g *heldTreeGroup) kill(cmd *exec.Cmd) <-chan struct{} {
	g.kills++
	return holdTreeCleanup(g.treeGroup.kill(cmd), g.killDone)
}
func (g *heldTreeGroup) sweep(cmd *exec.Cmd) <-chan struct{} {
	g.sweeps++
	return holdTreeCleanup(g.treeGroup.sweep(cmd), g.sweepDone)
}

func holdTreeCleanup(native, held <-chan struct{}) <-chan struct{} {
	if held == nil {
		return native
	}
	done := make(chan struct{})
	go func() {
		<-native
		<-held
		close(done)
	}()
	return done
}

func stillCleaning(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
		t.Errorf("%s completed before its group sweep finished", what)
	default:
	}
}

// WAN releases its port after both Stop and Wait. Neither may report cleanup
// complete while the Unix sweep's repeat signals are still in flight. Hold
// that completion explicitly, rather than relying on a scheduler pause.
func TestTreeWaitJoinsSweep(t *testing.T) {
	for _, before := range []bool{false, true} {
		name := "stop-after-reap"
		if before {
			name = "stop-before-wait"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tree := completionTree(t)
				release := make(chan struct{})
				g := &heldTreeGroup{treeGroup: tree.treeGroup, sweepDone: release}
				tree.treeGroup = g
				var once sync.Once
				finishSweep := func() { once.Do(func() { close(release) }) }
				finished := make(chan struct{})
				var waitErr error
				defer func() { finishSweep(); tree.Kill(); <-finished }()
				var stopped <-chan struct{}
				if before {
					stopped = tree.Stop(time.Hour)
				}
				go func() { waitErr = tree.Wait(); close(finished) }()
				synctest.Wait()
				if g.sweeps != 1 {
					t.Fatalf("Wait started %d sweeps, want one", g.sweeps)
				}
				stillCleaning(t, finished, "Wait")
				stillCleaning(t, tree.waited, "the waited notification")
				if stopped == nil {
					stopped = tree.Stop(time.Hour)
				}
				calls := make(chan (<-chan struct{}), 8)
				for range cap(calls) {
					go func() { calls <- tree.Stop(time.Minute) }()
				}
				synctest.Wait()
				for range cap(calls) {
					if next := <-calls; next != stopped {
						t.Error("concurrent Stop did not share its completion")
					}
				}
				tree.Kill() // a reaped leader is never signalled again; mu is free
				stillCleaning(t, stopped, "Stop")
				if g.kills != 0 {
					t.Errorf("Kill signalled a reaped leader %d times", g.kills)
				}
				finishSweep()
				synctest.Wait()
				<-finished
				<-stopped
				if waitErr != nil {
					t.Fatal(waitErr)
				}
			})
		})
	}
}

// A failed graceful signal (Windows), a command with no Wait, and a grace
// expiring all share the same kill completion. Wait also joins a kill begun
// earlier, rather than returning with its background sweep still running.
func TestTreeStopJoinsKill(t *testing.T) {
	for _, name := range []string{"grace-expired", "term-failed", "kill-before-stop"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tree := completionTree(t)
				release := make(chan struct{})
				g := &heldTreeGroup{treeGroup: tree.treeGroup, killDone: release}
				if name == "term-failed" {
					g.termErr = errors.ErrUnsupported
				}
				tree.treeGroup = g
				var once sync.Once
				finishKill := func() { once.Do(func() { close(release) }) }
				finished := make(chan struct{})
				defer func() { finishKill(); tree.Kill(); <-finished }()
				if name == "kill-before-stop" {
					tree.Kill()
				}
				stopped := tree.Stop(time.Second)
				synctest.Wait()
				// Let the grace expire without sleeping on the machine's clock.
				time.Sleep(time.Second)
				synctest.Wait()
				stillCleaning(t, stopped, "Stop after kill")
				tree.Kill()
				if g.kills != 1 {
					t.Errorf("started %d kill sweeps, want one", g.kills)
				}
				go func() { _ = tree.Wait(); close(finished) }()
				synctest.Wait()
				stillCleaning(t, finished, "Wait after kill")
				if tree.Stop(time.Hour) != stopped {
					t.Error("Stop after reap replaced its pending completion")
				}
				finishKill()
				synctest.Wait()
				<-stopped
				<-finished
			})
		})
	}
}
