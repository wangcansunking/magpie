package wan

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/proc"
)

// sdkHost is a fixed, bundled release of the official SDK, not a runtime
// package download. Credentials and proxy credentials go only through stdin.
//
//go:embed sdk/host.cjs
var sdkHost []byte

const sdkLineLimit = 64 << 10

type sdkMessage struct {
	Version      int           `json:"version"`
	Generation   string        `json:"generation"`
	Op           string        `json:"op"`
	Token        string        `json:"token"`
	AuthProvider string        `json:"authProvider,omitempty"`
	ClientID     string        `json:"clientID"`
	Config       *tunnelConfig `json:"config"`
	Port         int           `json:"port"`
	Fresh        bool          `json:"fresh"`
	Proxy        string        `json:"proxy"`
}

type sdkEvent struct {
	Version    int           `json:"version"`
	Generation string        `json:"generation"`
	Event      string        `json:"event"`
	Config     *tunnelConfig `json:"config,omitempty"`
	Endpoint   string        `json:"endpoint,omitempty"`
	Code       string        `json:"code,omitempty"`
	Revision   uint64        `json:"revision,omitempty"`
	Attempts   int           `json:"attempts,omitempty"`
	RetryInMS  int           `json:"retryInMs,omitempty"`
	Retrying   bool          `json:"retrying,omitempty"`
}

// availableRuntime never installs a runtime. InstallRuntime is the only WAN
// operation that may ask Bun to download one.
func cachedRuntimeAvailable(ctx context.Context) (bool, error) {
	path, err := plugin.CachedBunContext(ctx)
	return path != "", err
}

func availableRuntime(ctx context.Context) (string, error) {
	if ctx.Err() != nil {
		return "", errors.New("WAN runtime discovery was canceled")
	}
	path, err := plugin.CachedBunContext(ctx)
	if err != nil {
		return "", errors.New("WAN runtime discovery was canceled")
	}
	if path == "" {
		return "", errors.New("WAN runtime is unavailable; install the runtime before starting")
	}
	return path, nil
}

type sdkTree interface {
	Wait() error
	Stop(time.Duration) <-chan struct{}
}

func startSDKTree(cmd *exec.Cmd) (sdkTree, error) { return proc.StartTree(cmd) }

type sdkProcess struct {
	tree     sdkTree
	stdin    io.WriteCloser
	done     chan struct{}
	output   *sdkOutput
	writeMu  sync.Mutex
	stopOnce sync.Once
	folder   string
}

func startSDK(ctx context.Context, tool string, command func(string, ...string) *exec.Cmd, startTree func(*exec.Cmd) (sdkTree, error), generation string, config tunnelConfig, revoke func(), recoveryChanged func(*RecoveryStatus)) (*sdkProcess, error) {
	if ctx.Err() != nil {
		return nil, errors.New("WAN gateway startup was canceled")
	}
	folder, err := os.MkdirTemp("", "magpie-wan-sdk-")
	if err != nil {
		return nil, errors.New("Could not prepare the bundled WAN helper")
	}
	remove := true
	defer func() {
		if remove {
			_ = os.RemoveAll(folder)
		}
	}()
	script := filepath.Join(folder, "host.cjs")
	helper, err := sdkHelper()
	if err != nil {
		return nil, errors.New("Could not prepare the bundled WAN helper")
	}
	if err := os.WriteFile(script, helper, 0600); err != nil {
		return nil, errors.New("Could not prepare the bundled WAN helper")
	}
	p := &sdkProcess{done: make(chan struct{}), folder: folder}
	p.output = &sdkOutput{generation: generation, expected: config, prepared: make(chan sdkEvent, 1), ready: make(chan string, 1), bad: make(chan struct{}), revoke: revoke}
	p.output.recoveryChanged = recoveryChanged
	cmd := command(tool, "--no-env-file", script)
	cmd.Dir = folder
	cmd.Env = sdkEnvironment()
	ca, err := plugin.BunCAEnv(os.Environ(), folder)
	if err != nil {
		return nil, errors.New("WAN runtime certificate configuration is unavailable")
	}
	cmd.Env = append(cmd.Env, ca...)
	cmd.Stdout, cmd.Stderr = p.output, io.Discard
	cmd.WaitDelay = 2 * time.Second
	p.stdin, err = cmd.StdinPipe()
	if err != nil {
		return nil, errors.New("Could not open the WAN helper channel")
	}
	if ctx.Err() != nil {
		_ = p.stdin.Close()
		return nil, errors.New("WAN gateway startup was canceled")
	}
	p.tree, err = startTree(cmd)
	if err != nil {
		_ = p.stdin.Close()
		return nil, errors.New("WAN SDK helper could not be started")
	}
	remove = false
	go func() {
		_ = p.tree.Wait()
		p.output.finish()
		_ = p.stdin.Close()
		close(p.done)
	}()
	return p, nil
}

// Keep only the OS/runtime necessities. Inherited preload options, package
// resolution, dotenv and proxy settings must not change this fixed helper.
func sdkEnvironment() []string {
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		switch strings.ToUpper(key) {
		case "SYSTEMROOT", "WINDIR", "SYSTEMDRIVE", "TEMP", "TMP", "TMPDIR", "PATH", "PATHEXT", "LANG", "LC_ALL", "TZ":
			env = append(env, item)
		}
	}
	return env
}

type sdkControl struct {
	Version    int    `json:"version"`
	Generation string `json:"generation"`
	Op         string `json:"op"`
}

func (p *sdkProcess) send(ctx context.Context, msg any) error {
	if ctx.Err() != nil {
		return errors.New("WAN helper operation was canceled")
	}
	data, err := json.Marshal(msg)
	if err != nil || len(data)+1 > sdkLineLimit {
		return errors.New("WAN helper request is invalid")
	}
	data = append(data, '\n')
	defer clear(data)
	// Pipe writes are cancellable by closing the pipe. Always join the writer
	// before returning, so neither a token buffer nor a goroutine outlives it.
	done := make(chan error, 1)
	go func() {
		p.writeMu.Lock()
		defer p.writeMu.Unlock()
		if ctx.Err() != nil {
			done <- ctx.Err()
			return
		}
		_, err := p.stdin.Write(data)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			return errors.New("WAN helper channel closed")
		}
		return nil
	case <-ctx.Done():
		_ = p.stdin.Close()
		<-done
		return errors.New("WAN helper operation was canceled")
	case <-p.done:
		_ = p.stdin.Close()
		<-done
		return errors.New("WAN helper stopped unexpectedly")
	}
}

func (p *sdkProcess) stop() {
	p.stopOnce.Do(func() {
		p.output.closeAdmission()
		_ = p.stdin.Close() // EOF requests dispose, including remote endpoint cleanup.
		grace := time.NewTimer(900 * time.Millisecond)
		select {
		case <-p.done:
		case <-grace.C:
		}
		grace.Stop()
		<-p.tree.Stop(time.Second)
		<-p.done
		_ = os.RemoveAll(p.folder)
	})
}

// sdkOutput validates every stdout byte. A bad/disconnected message revokes
// admission in the reader itself, before the manager is scheduled again.
// Notifications are published only after the entire write has been checked.
// A ready message followed by malformed output in one write never admits work.
type sdkOutput struct {
	mu              sync.Mutex
	generation      string
	expected        tunnelConfig
	endpoint        string
	phase           int // 0 prepare, 1 prepared, 2 host requested, 3 ready
	line            []byte
	failed          error
	authRejected    bool
	pendingRevision uint64
	ackRevision     uint64
	refreshed       chan struct{}
	prepared        chan sdkEvent
	ready           chan string
	bad             chan struct{}
	revoke          func()
	recovery        *RecoveryStatus
	recoveryChanged func(*RecoveryStatus)
}

func (o *sdkOutput) closeAdmission() {
	if o.revoke != nil {
		o.revoke()
	}
}

func (o *sdkOutput) fail(err error) {
	if o.failed == nil {
		o.failed = err
		o.closeAdmission()
		close(o.bad)
	}
}

func (o *sdkOutput) authWasRejected() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.authRejected
}

func (o *sdkOutput) failure() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.failed
}

func (o *sdkOutput) finish() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.fail(errors.New("WAN SDK helper stopped; start WAN again explicitly"))
}

func (o *sdkOutput) host() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failed != nil {
		return o.failed
	}
	if o.phase != 1 {
		return errors.New("WAN helper did not prepare its tunnel")
	}
	o.phase = 2
	return nil
}

func (o *sdkOutput) credentials(revision uint64) (<-chan struct{}, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failed != nil {
		return nil, o.failed
	}
	if o.phase != 3 || o.pendingRevision != 0 || revision != o.ackRevision+1 {
		return nil, sdkError("protocol")
	}
	o.pendingRevision, o.refreshed = revision, make(chan struct{}, 1)
	return o.refreshed, nil
}

func (o *sdkOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var prepared *sdkEvent
	var ready string
	var refreshed bool
	for _, b := range p {
		if o.failed != nil {
			break
		}
		if b == '\n' {
			event, err := o.consume(o.line)
			o.line = o.line[:0]
			if err != nil {
				o.fail(err)
				break
			}
			if event.Event == "prepared" {
				prepared = &event
			} else if event.Event == "credentials" {
				refreshed = true
			} else if event.Event == "ready" {
				ready = event.Endpoint
			}
		} else if len(o.line) >= sdkLineLimit-1 {
			o.fail(errors.New("WAN helper returned unsupported protocol output"))
		} else {
			o.line = append(o.line, b)
		}
	}
	if o.failed == nil {
		if prepared != nil {
			o.prepared <- *prepared
		}
		if ready != "" {
			o.ready <- ready
		}
		if o.recoveryChanged != nil {
			o.recoveryChanged(o.recovery)
		}
		if refreshed {
			o.refreshed <- struct{}{}
		}
	}
	return len(p), nil
}

func (o *sdkOutput) consume(line []byte) (sdkEvent, error) {
	bad := errors.New("WAN helper returned unsupported protocol output")
	var event sdkEvent
	if strictJSON(line, &event) != nil || event.Version != 1 || event.Generation != o.generation || !validEventFields(line, event.Event) {
		return event, bad
	}
	switch event.Event {
	case "prepared":
		if o.phase != 0 || event.Config == nil || event.Code != "" || (!validCanonicalConfig(*event.Config) || event.Config.AccountID != "" || event.Config.AccountName != "" || event.Config.New) || event.Config.Port != o.expected.Port {
			return event, bad
		}
		if o.expected.Version == 2 && (event.Config.ID != o.expected.ID || event.Config.ClusterID != o.expected.ClusterID) {
			return event, bad
		}
		if event.Endpoint != "" {
			u, err := endpointURL(event.Endpoint, event.Config.Port)
			if err != nil {
				return event, bad
			}
			event.Endpoint = strings.TrimSuffix(u.String(), "/")
		}
		o.endpoint, o.phase = event.Endpoint, 1
	case "ready":
		if o.phase != 2 || event.Config != nil || event.Code != "" {
			return event, bad
		}
		u, err := endpointURL(event.Endpoint, o.expected.Port)
		if err != nil {
			return event, bad
		}
		event.Endpoint = strings.TrimSuffix(u.String(), "/")
		if o.endpoint != "" && o.endpoint != event.Endpoint {
			return event, bad
		}
		o.phase = 3
	case "credentials":
		if o.phase != 3 || event.Revision == 0 || event.Revision != o.pendingRevision {
			return event, bad
		}
		o.ackRevision, o.pendingRevision = event.Revision, 0
	case "recovery":
		if o.phase != 3 || event.Attempts < 0 || event.Attempts > 15 || event.RetryInMS < 0 || event.RetryInMS > 20000 || (event.Retrying && (event.Attempts == 0 || event.RetryInMS != 0)) {
			return event, bad
		}
		if o.recovery != nil && (event.Attempts < o.recovery.Attempts || event.Attempts > o.recovery.Attempts+1) {
			return event, bad
		}
		if o.recovery == nil && event.Attempts != 0 {
			return event, bad
		}
		o.recovery = &RecoveryStatus{Attempts: event.Attempts, Retrying: event.Retrying, NextRetryAt: time.Now().Add(time.Duration(event.RetryInMS) * time.Millisecond)}
	case "recovered":
		if o.phase != 3 || o.recovery == nil {
			return event, bad
		}
		o.recovery = nil
	case "disconnected", "error":
		if event.Config != nil || event.Endpoint != "" {
			return event, bad
		}
		o.authRejected = event.Event == "error" && event.Code == "auth"
		return event, sdkError(event.Code)
	default:
		return event, bad
	}
	return event, nil
}

func validEventFields(data []byte, event string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return false
	}
	allowed := map[string]bool{"version": true, "generation": true, "event": true}
	switch event {
	case "prepared":
		allowed["config"], allowed["endpoint"] = true, true
		var config map[string]json.RawMessage
		if json.Unmarshal(fields["config"], &config) != nil || len(config) != 4 {
			return false
		}
		for key := range config {
			if key != "version" && key != "id" && key != "clusterId" && key != "port" {
				return false
			}
		}
	case "ready":
		allowed["endpoint"] = true
	case "credentials":
		allowed["revision"] = true
	case "recovery":
		if len(fields) != 6 || bytes.Equal(fields["attempts"], []byte("null")) || bytes.Equal(fields["retryInMs"], []byte("null")) || bytes.Equal(fields["retrying"], []byte("null")) {
			return false
		}
		allowed["attempts"], allowed["retryInMs"], allowed["retrying"] = true, true, true
	case "recovered":
	case "error", "disconnected":
		allowed["code"] = true
	default:
		return false
	}
	for key := range fields {
		if !allowed[key] {
			return false
		}
	}
	return true
}

func sdkError(code string) error {
	switch code {
	case "auth":
		return errors.New("WAN sign-in was rejected; sign in again before starting")
	case "forbidden":
		return errors.New("WAN tunnel service refused permission or policy; check the account and service")
	case "not_found":
		return errors.New("The saved WAN tunnel was not found for this account; it was not replaced")
	case "ports":
		return errors.New("WAN tunnel must contain exactly its saved HTTP port; hosting was refused")
	case "proxy":
		return errors.New("WAN proxy is unavailable or unsupported; no direct connection was attempted")
	case "disconnected":
		return errors.New("WAN relay disconnected; start WAN again explicitly")
	case "reconnect_timeout":
		return errors.New(RelayRecoveryTimeoutMessage)
	case "host_conflict":
		return errors.New("Another host connected to this WAN tunnel; stop the other host or choose a new tunnel")
	case "bad_request":
		return errors.New("WAN service rejected the tunnel request (HTTP 400)")
	case "conflict":
		return errors.New("WAN tunnel identity conflicts with the service (HTTP 409)")
	case "rate_limit":
		return errors.New("WAN service rate limit reached (HTTP 429); try later")
	case "service":
		return errors.New("WAN tunnel service is temporarily unavailable (HTTP 5xx)")
	case "network":
		return errors.New("WAN service connection failed; check DNS, network and proxy settings")
	case "tls":
		return errors.New("WAN service TLS verification failed; check trusted certificates")
	case "timeout":
		return errors.New("WAN service request timed out; check network and proxy settings")
	case "runtime":
		return errors.New("WAN SDK runtime operation failed before a valid service result")
	case "create":
		return errors.New("WAN SDK could not create the tunnel")
	case "read":
		return errors.New("WAN SDK could not read the tunnel")
	case "update":
		return errors.New("WAN SDK could not configure the tunnel port")
	case "identity":
		return errors.New("WAN service returned an unexpected tunnel identity")
	case "port_options":
		return errors.New("WAN service did not confirm the required HTTP port options")
	case "host_token":
		return errors.New("WAN service did not return the required hosting credential")
	case "redirect":
		return errors.New("WAN service redirected outside the allowed management hosts")
	case "prepare", "host", "protocol", "dispose":
		return errors.New("WAN SDK operation failed; start WAN again explicitly")
	default:
		return errors.New("WAN helper returned unsupported protocol output")
	}
}

// encoding/json deliberately accepts duplicate object keys. Neither persisted
// identity nor private control messages may rely on which duplicate wins.
func strictJSON(data []byte, target any) error {
	check := json.NewDecoder(bytes.NewReader(data))
	var value func() error
	value = func() error {
		token, err := check.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := make(map[string]bool)
			for check.More() {
				key, err := check.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] {
					return errors.New("invalid JSON object")
				}
				seen[name] = true
				if err := value(); err != nil {
					return err
				}
			}
		case '[':
			for check.More() {
				if err := value(); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid JSON value")
		}
		_, err = check.Token()
		return err
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := check.Token(); err != io.EOF {
		return errors.New("unexpected JSON content")
	}
	if err := exactJSONFields(data, reflect.TypeOf(target)); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

// DisallowUnknownFields still accepts differently cased aliases. Control
// messages have exact field names and no null/array/object type substitutions.
func exactJSONFields(data []byte, typ reflect.Type) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		return errors.New("JSON object required")
	}
	fields := make(map[string]reflect.Type)
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		fields[name] = field.Type
	}
	for key, value := range object {
		field, ok := fields[key]
		if !ok {
			return errors.New("unknown JSON field")
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			if field.Kind() != reflect.Pointer {
				return errors.New("null JSON field")
			}
			continue
		}
		if err := exactJSONFields(value, field); err != nil {
			return err
		}
	}
	return nil
}
