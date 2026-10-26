package wan

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testSDKOutput() *sdkOutput {
	return &sdkOutput{
		generation: strings.Repeat("a", 32), expected: tunnelConfig{Port: 8080},
		prepared: make(chan sdkEvent, 1), ready: make(chan string, 1), bad: make(chan struct{}),
	}
}

func writeSDKEvent(t *testing.T, out *sdkOutput, event sdkEvent) {
	t.Helper()
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	out.Write(append(data, '\n'))
}

func preparedEvent() sdkEvent {
	return sdkEvent{Version: 1, Generation: strings.Repeat("a", 32), Event: "prepared", Config: &tunnelConfig{Version: 2, ID: "actual-id", ClusterID: "usw2", Port: 8080}, Endpoint: "https://actual-id-8080.usw2.devtunnels.ms"}
}

func TestSDKReadyRequiresPreparePersistHostOrdering(t *testing.T) {
	out := testSDKOutput()
	writeSDKEvent(t, out, preparedEvent())
	select {
	case <-out.prepared:
	default:
		t.Fatal("valid canonical prepare was not delivered")
	}
	select {
	case <-out.ready:
		t.Fatal("prepare opened readiness")
	default:
	}
	if err := out.host(); err != nil {
		t.Fatal(err)
	}
	writeSDKEvent(t, out, sdkEvent{Version: 1, Generation: out.generation, Event: "ready", Endpoint: preparedEvent().Endpoint + "/"})
	select {
	case endpoint := <-out.ready:
		if endpoint != preparedEvent().Endpoint {
			t.Fatal(endpoint)
		}
	default:
		t.Fatal("ready was not delivered")
	}
}

func TestSDKMalformedMessagesFailClosed(t *testing.T) {
	valid, _ := json.Marshal(preparedEvent())
	for name, input := range map[string]string{
		"empty": "\n", "null": "null\n", "unknown key": strings.TrimSuffix(string(valid), "}") + `,"token":"SECRET"}` + "\n",
		"duplicate key":        strings.TrimSuffix(string(valid), "}") + `,"version":1}` + "\n",
		"stale generation":     strings.ReplaceAll(string(valid), strings.Repeat("a", 32), strings.Repeat("b", 32)) + "\n",
		"unknown version":      strings.Replace(string(valid), `"version":1`, `"version":2`, 1) + "\n",
		"alias field":          strings.Replace(string(valid), `"version":1`, `"Version":1`, 1) + "\n",
		"null field":           strings.TrimSuffix(string(valid), "}") + `,"code":null}` + "\n",
		"wrong port":           strings.ReplaceAll(string(valid), "8080", "8081") + "\n",
		"bad canonical id":     strings.Replace(string(valid), "actual-id", "UPPER-ID", 1) + "\n",
		"bad cluster":          strings.ReplaceAll(string(valid), "usw2", "attacker") + "\n",
		"other endpoint":       strings.ReplaceAll(string(valid), "devtunnels.ms", "attacker.example") + "\n",
		"unknown event":        strings.ReplaceAll(string(valid), "prepared", "reconnect") + "\n",
		"ready before prepare": `{"version":1,"generation":"` + strings.Repeat("a", 32) + `","event":"ready","endpoint":"https://actual-id-8080.usw2.devtunnels.ms"}` + "\n",
		"oversized":            strings.Repeat("SECRET", sdkLineLimit),
		"error injection":      `{"version":1,"generation":"` + strings.Repeat("a", 32) + `","event":"error","code":"SECRET-TOKEN"}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			out := testSDKOutput()
			var revoked atomic.Bool
			out.revoke = func() { revoked.Store(true) }
			out.Write([]byte(input))
			if !revoked.Load() {
				t.Fatal("malformed message did not immediately revoke admission")
			}
			select {
			case <-out.bad:
			default:
				t.Fatal("malformed message did not fail")
			}
			if err := out.failure(); err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("unsanitized failure %v", err)
			}
			if len(out.line) > sdkLineLimit {
				t.Fatal("unbounded IPC buffer")
			}
			select {
			case <-out.prepared:
				t.Fatal("malformed message prepared hosting")
			default:
			}
		})
	}
}

func TestSDKReadyThenBadSameWriteNeverBecomesReady(t *testing.T) {
	out := testSDKOutput()
	writeSDKEvent(t, out, preparedEvent())
	<-out.prepared
	if err := out.host(); err != nil {
		t.Fatal(err)
	}
	ready, _ := json.Marshal(sdkEvent{Version: 1, Generation: out.generation, Event: "ready", Endpoint: preparedEvent().Endpoint})
	out.Write(append(ready, []byte("\n{\"unknown\":true}\n")...))
	select {
	case <-out.ready:
		t.Fatal("ready escaped later failure in the same read")
	default:
	}
	if out.failure() == nil {
		t.Fatal("malformed output did not fail")
	}
}

func TestSDKDisconnectImmediatelyRevokesAndNeverReadmits(t *testing.T) {
	out := testSDKOutput()
	var admitted atomic.Bool
	admitted.Store(true)
	out.revoke = func() { admitted.Store(false) }
	writeSDKEvent(t, out, sdkEvent{Version: 1, Generation: out.generation, Event: "disconnected", Code: "disconnected"})
	if admitted.Load() {
		t.Fatal("disconnect left admission open")
	}
	writeSDKEvent(t, out, sdkEvent{Version: 1, Generation: out.generation, Event: "ready", Endpoint: preparedEvent().Endpoint})
	if admitted.Load() {
		t.Fatal("late ready revived admission")
	}
	select {
	case <-out.ready:
		t.Fatal("disconnected generation delivered readiness")
	default:
	}
}

func TestSDKRecoveryTimeoutRevokesAndCannotBeReadmitted(t *testing.T) {
	out := testSDKOutput()
	var admitted atomic.Bool
	admitted.Store(true)
	out.revoke = func() { admitted.Store(false) }
	writeSDKEvent(t, out, sdkEvent{Version: 1, Generation: out.generation, Event: "error", Code: "reconnect_timeout"})
	if admitted.Load() || out.failure() == nil || out.failure().Error() != RelayRecoveryTimeoutMessage {
		t.Fatal("recovery exhaustion did not terminate admission with its specific error")
	}
	writeSDKEvent(t, out, sdkEvent{Version: 1, Generation: out.generation, Event: "ready", Endpoint: preparedEvent().Endpoint})
	if admitted.Load() {
		t.Fatal("late SDK readiness revived an exhausted recovery")
	}
}

func TestSDKCannotAdoptDifferentSavedCanonicalIdentity(t *testing.T) {
	for _, change := range []func(*tunnelConfig){func(c *tunnelConfig) { c.ID = "different-id" }, func(c *tunnelConfig) { c.ClusterID = "use2" }} {
		out := testSDKOutput()
		out.expected = *preparedEvent().Config
		event := preparedEvent()
		change(event.Config)
		writeSDKEvent(t, out, event)
		if out.failure() == nil {
			t.Fatal("adopted a different canonical tunnel")
		}
	}
}

func TestSDKRecoveryProgressProjectsStatusWithoutRevoking(t *testing.T) {
	out := testSDKOutput()
	out.phase = 3
	r := &hosting{}
	r.admitted.Store(true)
	out.revoke = r.revoke
	out.recoveryChanged = r.recovery.Store
	m := &Manager{status: Status{State: "running", Endpoint: preparedEvent().Endpoint}, current: r}
	send := func(fields string) {
		out.Write([]byte(`{"version":1,"generation":"` + out.generation + `",` + fields + "\n"))
	}
	send(`"event":"recovery","attempts":0,"retryInMs":0,"retrying":false}`)
	if s := m.Status(); s.State != "reconnecting" || s.Recovery == nil || s.Recovery.Attempts != 0 || s.Endpoint == "" {
		t.Fatalf("missing reconnect status: %+v", s)
	}
	send(`"event":"recovery","attempts":1,"retryInMs":0,"retrying":true}`)
	if s := m.Status(); !s.Recovery.Retrying || s.Recovery.Attempts != 1 {
		t.Fatal(s.Recovery)
	}
	send(`"event":"recovery","attempts":1,"retryInMs":20000,"retrying":false}`)
	s := m.Status()
	remaining := s.Recovery.NextRetryAt.Sub(s.Recovery.ServerTime)
	if remaining <= 0 || remaining > 20*time.Second {
		t.Fatal(remaining)
	}
	s.Recovery.Attempts = 99
	if m.Status().Recovery.Attempts != 1 {
		t.Fatal("status exposed mutable recovery")
	}
	if !r.admitted.Load() || out.failed != nil {
		t.Fatal("transient outage revoked the credential lease", out.failed)
	}
	send(`"event":"recovered"}`)
	if s = m.Status(); s.State != "running" || s.Recovery != nil {
		t.Fatalf("recovery did not clear: %+v", s)
	}
	send(`"event":"recovery","attempts":0,"retryInMs":0,"retrying":false}`)
	writeSDKEvent(t, out, sdkEvent{Version: 1, Generation: out.generation, Event: "disconnected", Code: "reconnect_timeout"})
	if r.admitted.Load() || out.failed == nil {
		t.Fatal("terminal timeout did not revoke")
	}
	m.status.State = "failed"
	if s = m.Status(); s.Recovery != nil || s.State != "failed" {
		t.Fatal("failed status retained recovery", s)
	}
}

func TestSDKRecoveryProgressRejectsInvalidOrStaleOutput(t *testing.T) {
	for _, fields := range []string{
		`"attempts":null,"retryInMs":0,"retrying":false`,
		`"attempts":0,"retryInMs":null,"retrying":false`,
		`"attempts":0,"retryInMs":0,"retrying":null`,
		`"attempts":16,"retryInMs":0,"retrying":false`,
		`"attempts":0,"retryInMs":20001,"retrying":false`,
		`"attempts":0,"retryInMs":0,"retrying":true`,
		`"attempts":2,"retryInMs":0,"retrying":true`,
		`"attempts":0,"retryInMs":0,"retrying":false,"token":"forbidden"`,
	} {
		t.Run(fields, func(t *testing.T) {
			out := testSDKOutput()
			out.phase = 3
			var progress atomic.Pointer[RecoveryStatus]
			out.recoveryChanged = progress.Store
			out.Write([]byte(`{"version":1,"generation":"` + out.generation + `","event":"recovery",` + fields + "}\n"))
			if out.failed == nil || progress.Load() != nil {
				t.Fatal("invalid progress was published")
			}
		})
	}
	out := testSDKOutput()
	out.phase = 3
	out.Write([]byte(`{"version":1,"generation":"` + out.generation + `","event":"recovery","attempts":0,"retryInMs":0,"retrying":false}` + "\n"))
	m := &Manager{status: Status{State: "running"}, current: &hosting{}}
	retired := &hosting{}
	out.recoveryChanged = retired.recovery.Store
	out.Write([]byte(`{"version":1,"generation":"` + out.generation + `","event":"recovery","attempts":1,"retryInMs":0,"retrying":true}` + "\n"))
	if m.Status().Recovery != nil {
		t.Fatal("retired generation changed current status")
	}
}
