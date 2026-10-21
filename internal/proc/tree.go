package proc

import (
	"os/exec"
	"sync"
	"time"
)

// A Tree is a started command and the processes it starts in turn: an
// agent's CLI with the MCP servers, shells and helpers it runs. Killing the
// CLI alone leaves those running, so they are ended with it.
type Tree struct {
	cmd *exec.Cmd

	mu      sync.Mutex
	done    bool            // command reaped: its pid may be another process's now
	waited  chan struct{}   // closed once Wait and its cleanup sweeps have ended
	stopped chan struct{}   // Stop's, closed once its termination sweep has ended
	killing <-chan struct{} // the one kill sweep, if Kill or Stop started it
	treeGroup
}

// Each tree owns its group's cleanup; no global callback can outlive its owner.
type treeGroup interface {
	join(*exec.Cmd)
	kill(*exec.Cmd) <-chan struct{}
	term(*exec.Cmd) error
	sweep(*exec.Cmd) <-chan struct{}
}

// StartTree starts cmd in a group of its own: a session on Unix, a job on
// Windows. It has no terminal then, and a Ctrl+C there doesn't reach it.
func StartTree(cmd *exec.Cmd) (*Tree, error) {
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	t := &Tree{cmd: cmd, waited: make(chan struct{}), treeGroup: &group{}}
	t.join(cmd)
	return t, nil
}

// Wait waits for the command and the cleanup of what it left running. On
// Unix a sweep ends when the group is gone or its existing killFor bound
// expires; returning never leaves that sweep's goroutine behind.
func (t *Tree) Wait() error {
	err := t.cmd.Wait()
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		<-t.waited
		return err
	}
	// Mark the leader reaped before letting Stop or Kill take the lock again:
	// its pid must not be signalled after this point. Sweeps target the group.
	t.done = true
	swept, killing := t.sweep(t.cmd), t.killing
	t.mu.Unlock()
	<-swept
	if killing != nil {
		<-killing
	}
	close(t.waited)
	return err
}

// Kill starts ending the command and every process of its tree. Wait joins
// the cleanup; Kill itself remains nonblocking.
func (t *Tree) Kill() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.killLocked()
}

// killLocked starts at most one kill sweep. Once the leader is reaped, its
// Wait already owns cleanup and no signal may go to that pid again.
func (t *Tree) killLocked() <-chan struct{} {
	if t.done {
		return t.waited
	}
	if t.killing == nil {
		t.killing = t.kill(t.cmd)
		_ = t.cmd.Process.Kill()
	}
	return t.killing
}

// Stop asks the command to end, so it can clean up first, and ends its tree
// once it has (Wait sweeps what it left) or grace has passed (Kill). Claude
// Code holding its OAuth refresh lock removes it as it exits on SIGTERM, and
// a SIGKILL left ~/.claude/.oauth_refresh.lock behind for every later
// request to wait on (#2195). Windows has no such signal: the tree is killed
// at once there. It returns at once; the channel closes once its cleanup
// sweep has ended (subject to killFor on Unix). A tree no one Waits for is
// killed after grace.
func (t *Tree) Stop(grace time.Duration) <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped != nil {
		return t.stopped
	}
	t.stopped = make(chan struct{})
	var ending <-chan struct{}
	switch {
	case t.done:
		ending = t.waited
	case t.killing != nil:
		ending = t.killing
	case t.term(t.cmd) != nil:
		ending = t.killLocked()
	}
	go func() {
		defer close(t.stopped)
		if ending != nil {
			<-ending
			return
		}
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-t.waited:
		case <-timer.C:
			t.mu.Lock()
			ending := t.killLocked()
			t.mu.Unlock()
			<-ending
		}
	}()
	return t.stopped
}
