package wan

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yetone/magpie/internal/filelock"
)

const diagnosticLimit = 1 << 20

// Never persist raw helper output, credentials, account names, URLs or vendor errors.
type diagnosticRecord struct {
	Time      time.Time `json:"time"`
	Run       string    `json:"run"`
	Event     string    `json:"event"`
	Code      string    `json:"code,omitempty"`
	Cause     string    `json:"cause,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Attempts  int       `json:"attempts"`
	ElapsedMS int       `json:"elapsedMs,omitempty"`
	RetryInMS int       `json:"retryInMs,omitempty"`
	Dropped   uint64    `json:"dropped,omitempty"`
}

type diagnosticLog struct {
	mu      sync.Mutex
	closed  bool
	queue   chan diagnosticRecord
	done    chan struct{}
	dropped atomic.Uint64
	final   *diagnosticRecord
	err     error // worker-owned; read only after done closes
}

func newDiagnosticLog(path string) *diagnosticLog {
	return startDiagnosticLog(func(record diagnosticRecord) error { return appendDiagnostic(path, record, diagnosticLimit) })
}

func startDiagnosticLog(appendRecord func(diagnosticRecord) error) *diagnosticLog {
	d := &diagnosticLog{queue: make(chan diagnosticRecord, 256), done: make(chan struct{})}
	run := fmt.Sprintf("%x-%x", time.Now().UnixNano(), os.Getpid())
	go func() {
		defer close(d.done)
		write := func(record diagnosticRecord) {
			if d.err != nil {
				return
			}
			record.Run = run
			if err := appendRecord(record); err != nil && d.err == nil {
				d.err = err
				log.Print("WAN diagnostic log could not be saved; check settings folder permissions and disk space")
			}
		}
		lost := func() {
			if n := d.dropped.Swap(0); n != 0 {
				write(diagnosticRecord{Time: time.Now().UTC(), Event: "events_dropped", Dropped: n})
			}
		}
		for record := range d.queue {
			lost()
			write(record)
		}
		lost()
		if d.final != nil {
			write(*d.final)
		}
	}()
	return d
}

// Disk I/O never runs under the protocol/credential lock. A bounded queue keeps
// malformed-output revocation independent of a slow diagnostic disk.
func (d *diagnosticLog) record(record diagnosticRecord) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	record.Time = time.Now().UTC()
	select {
	case d.queue <- record:
	default:
		d.dropped.Add(1)
	}
}

// Called after the helper has joined, outside manager/runtime locks.
func (d *diagnosticLog) close(final ...diagnosticRecord) error {
	d.mu.Lock()
	if !d.closed {
		if len(final) > 0 {
			record := final[0]
			record.Time = time.Now().UTC()
			d.final = &record
		}
		d.closed = true
		close(d.queue)
	}
	d.mu.Unlock()
	<-d.done
	return d.err
}

func appendDiagnostic(path string, record diagnosticRecord, limit int64) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if int64(len(data)) > limit {
		return errors.New("WAN diagnostic record exceeds log limit")
	}
	unlock, held := filelock.Lock(path+".lock", 100*time.Millisecond)
	if !held {
		return errors.New("WAN diagnostic file lock is unavailable")
	}
	defer unlock()
	info, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if info != nil {
		if !info.Mode().IsRegular() {
			return errors.New("WAN diagnostic path is not a regular file")
		}
		if info.Size()+int64(len(data)) > limit {
			if err := os.Remove(path + ".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := os.Rename(path, path+".1"); err != nil {
				return err
			}
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	return errors.Join(writeErr, f.Close())
}

func diagnosticFailure(err error) (string, string) {
	if err == nil {
		return "", ""
	}
	for _, code := range diagnosticSDKCodes {
		reason := sdkError(code).Error()
		if err.Error() == reason {
			return code, reason
		}
	}
	if diagnosticLocalErrors[err.Error()] {
		return "local", err.Error()
	}
	return "unclassified", "WAN operation failed; unrecognized error details were omitted"
}

var diagnosticSDKCodes = []string{"auth", "forbidden", "not_found", "ports", "proxy", "disconnected", "reconnect_timeout", "host_conflict", "bad_request", "conflict", "rate_limit", "service", "network", "tls", "timeout", "runtime", "create", "read", "update", "identity", "port_options", "host_token", "redirect", "protocol", "prepare", "host", "dispose", "protocol_output"}

func diagnosticCode(code string) bool {
	for _, known := range diagnosticSDKCodes {
		if code == known {
			return true
		}
	}
	return false
}
func diagnosticPath(config string) string {
	return filepath.Join(filepath.Dir(config), "wan-diagnostics.jsonl")
}

var diagnosticLocalErrors = map[string]bool{
	"A valid WAN GitHub OAuth Client ID is required":                                             true,
	"Another backend owns WAN gateway, or its runtime lock is unavailable":                       true,
	"Another host connected to this WAN tunnel; stop the other host or choose a new tunnel":      true,
	"Could not generate a private WAN hosting generation":                                        true,
	"Could not identify the WAN account; sign in again":                                          true,
	"Could not install the WAN runtime":                                                          true,
	"Could not open the WAN helper channel":                                                      true,
	"Could not prepare the bundled WAN helper":                                                   true,
	"Could not read wan.json; refusing to replace the saved tunnel":                              true,
	"Could not save WAN tunnel identity; no tunnel was hosted":                                   true,
	"Credentials expired before WAN renewal; start WAN again after sign-in recovers":             true,
	"Dev Tunnels account bandwidth is exhausted; wait for the reset or request a limit increase": true,
	"Invalid wan.json; refusing to replace the saved tunnel":                                     true,
	"Invalidate WAN sign-in before erasing its saved credential":                                 true,
	"JSON object required":                                                           true,
	"Sign in to GitHub for WAN before starting the gateway":                          true,
	"Stop WAN gateway before replacing its sign-in":                                  true,
	"The saved WAN tunnel was not found for this account; it was not replaced":       true,
	"Unsupported WAN login provider":                                                 true,
	"Unsupported WAN sign-in method":                                                 true,
	"WAN SDK could not configure the tunnel port":                                    true,
	"WAN SDK could not create the tunnel":                                            true,
	"WAN SDK could not read the tunnel":                                              true,
	"WAN SDK helper could not be started":                                            true,
	"WAN SDK helper exited before preparation":                                       true,
	"WAN SDK helper exited before readiness":                                         true,
	"WAN SDK helper stopped unexpectedly; WAN gateway is offline":                    true,
	"WAN SDK helper stopped; start WAN again explicitly":                             true,
	"WAN SDK operation failed; start WAN again explicitly":                           true,
	"WAN SDK runtime operation failed before a valid service result":                 true,
	"WAN authorization cannot start while magpie is shutting down":                   true,
	"WAN authorization owner is not running":                                         true,
	"WAN authorization owner is unavailable or busy":                                 true,
	"WAN gateway already has an owner":                                               true,
	"WAN gateway cannot start while magpie is shutting down":                         true,
	"WAN gateway handler is unavailable":                                             true,
	"WAN gateway is stopping; try again after it stops":                              true,
	"WAN gateway owner is not running":                                               true,
	"WAN gateway owner is not running or is busy":                                    true,
	"WAN gateway preparation was canceled or failed":                                 true,
	"WAN gateway startup deadline expired":                                           true,
	"WAN gateway startup was canceled":                                               true,
	"WAN gateway's saved loopback port is unavailable; no tunnel was hosted":         true,
	"WAN helper channel closed":                                                      true,
	"WAN helper did not prepare its tunnel":                                          true,
	"WAN helper operation was canceled":                                              true,
	"WAN helper request is invalid":                                                  true,
	"WAN helper returned unsupported protocol output":                                true,
	"WAN helper stopped unexpectedly":                                                true,
	"WAN listener stopped unexpectedly":                                              true,
	"WAN proxy configuration is unavailable":                                         true,
	"WAN proxy is unavailable or unsupported; no direct connection was attempted":    true,
	"WAN proxy stopped unexpectedly; WAN gateway is offline":                         true,
	"WAN relay disconnected; start WAN again explicitly":                             true,
	"WAN runtime cannot install while magpie is shutting down":                       true,
	"WAN runtime certificate configuration is unavailable":                           true,
	"WAN runtime discovery was canceled":                                             true,
	"WAN runtime is unavailable; install the runtime before starting":                true,
	"WAN runtime owner is unavailable or busy":                                       true,
	"WAN service TLS verification failed; check trusted certificates":                true,
	"WAN service connection failed; check DNS, network and proxy settings":           true,
	"WAN service did not confirm the required HTTP port options":                     true,
	"WAN service did not return the required hosting credential":                     true,
	"WAN service rate limit reached (HTTP 429); try later":                           true,
	"WAN service redirected outside the allowed management hosts":                    true,
	"WAN service rejected the tunnel request (HTTP 400)":                             true,
	"WAN service request timed out; check network and proxy settings":                true,
	"WAN service returned an unexpected tunnel identity":                             true,
	"WAN sign-in changed; retry the operation":                                       true,
	"WAN sign-in expired; sign in again":                                             true,
	"WAN sign-in is loading or signing out; finish that operation first":             true,
	"WAN sign-in is no longer available":                                             true,
	"WAN sign-in is unavailable or busy":                                             true,
	"WAN sign-in was rejected; sign in again before starting":                        true,
	"WAN startup or sign-in expired before hosting":                                  true,
	"WAN startup or sign-in expired before readiness":                                true,
	"WAN tunnel identity conflicts with the service (HTTP 409)":                      true,
	"WAN tunnel must contain exactly its saved HTTP port; hosting was refused":       true,
	"WAN tunnel service is temporarily unavailable (HTTP 5xx)":                       true,
	"WAN tunnel service refused permission or policy; check the account and service": true,
	"invalid JSON object":                                                            true,
	"invalid JSON value":                                                             true,
	"null JSON field":                                                                true,
	"unexpected JSON content":                                                        true,
	"unknown JSON field":                                                             true,
}

// Bounded transport identifiers carry errno/status/reason, never an error message.
func diagnosticCause(cause string) bool {
	if cause == "" || cause == "unspecified" {
		return true
	}
	for _, value := range []string{"ETIMEDOUT", "ECONNABORTED", "ERR_CANCELED", "ENOTFOUND", "EAI_AGAIN", "ECONNREFUSED", "ECONNRESET", "ENETUNREACH", "EHOSTUNREACH", "CERT_HAS_EXPIRED", "DEPTH_ZERO_SELF_SIGNED_CERT", "SELF_SIGNED_CERT_IN_CHAIN", "UNABLE_TO_VERIFY_LEAF_SIGNATURE", "UNABLE_TO_GET_ISSUER_CERT_LOCALLY", "ERR_TLS_CERT_ALTNAME_INVALID"} {
		if cause == value {
			return true
		}
	}
	for n := 400; n <= 599; n++ {
		if cause == fmt.Sprintf("http_%d", n) {
			return true
		}
	}
	for n := 1; n <= 15; n++ {
		if cause == fmt.Sprintf("ssh_%d", n) {
			return true
		}
	}
	return false
}
