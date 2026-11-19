package wan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func readDiagnostics(t *testing.T, path string) []diagnosticRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var records []diagnosticRecord
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record diagnosticRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal("invalid journal line", err)
		}
		records = append(records, record)
	}
	return records
}

func TestWANDiagnosticsPersistAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "wan-diagnostics.jsonl")
	for _, event := range []string{"starting", "stopped"} {
		d := newDiagnosticLog(path)
		d.record(diagnosticRecord{Event: event})
		if err := d.close(); err != nil {
			t.Fatal(err)
		}
	}
	rows := readDiagnostics(t, path)
	if len(rows) != 2 || rows[0].Event != "starting" || rows[1].Event != "stopped" || rows[0].Run == rows[1].Run || rows[0].Time.IsZero() {
		t.Fatal(rows)
	}
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		t.Fatal(info, err)
	}
}

func TestWANDiagnosticsRotateWholeRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wan-diagnostics.jsonl")
	const limit = 512
	for i := 0; i < 30; i++ {
		if err := appendDiagnostic(path, diagnosticRecord{Time: time.Now().UTC(), Run: "fixture", Event: "retry_failed", Code: "network", Attempts: i}, limit); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{path, path + ".1"} {
		info, err := os.Stat(file)
		if err != nil || info.Size() > limit {
			t.Fatal(info, err)
		}
		if len(readDiagnostics(t, file)) == 0 {
			t.Fatal("rotation lost all records")
		}
	}
	before, _ := os.ReadFile(path)
	if err := appendDiagnostic(path, diagnosticRecord{Reason: strings.Repeat("x", limit)}, limit); err == nil {
		t.Fatal("oversized record accepted")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("oversized record changed existing log")
	}
}

func TestWANDiagnosticsRecordEveryRetryAndSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wan-diagnostics.jsonl")
	d := newDiagnosticLog(path)
	t.Cleanup(func() { _ = d.close() })
	out := testSDKOutput()
	out.phase = 3
	out.journal = d
	send := func(body string) {
		out.Write([]byte(`{"version":1,"generation":"` + out.generation + `",` + body + "\n"))
	}
	send(`"event":"diagnostic","kind":"relay_disconnected","code":"network","attempts":0,"elapsedMs":0}`)
	send(`"event":"recovery","attempts":0,"retryInMs":0,"retrying":false}`)
	for i := 1; i <= 2; i++ {
		send(fmt.Sprintf(`"event":"recovery","attempts":%d,"retryInMs":0,"retrying":true}`, i))
		if i == 1 {
			send(`"event":"diagnostic","kind":"retry_failed","code":"service","attempts":1,"elapsedMs":11}`)
			send(`"event":"recovery","attempts":1,"retryInMs":19990,"retrying":false}`)
		}
	}
	send(`"event":"recovered"}`)
	if out.failed != nil {
		t.Fatal(out.failed)
	}
	if err := d.close(); err != nil {
		t.Fatal(err)
	}
	rows := readDiagnostics(t, path)
	var events []string
	for _, r := range rows {
		events = append(events, r.Event)
	}
	want := "relay_disconnected,retry_scheduled,retry_started,retry_failed,retry_scheduled,retry_started,recovered"
	if strings.Join(events, ",") != want {
		t.Fatal(events)
	}
	if rows[2].Attempts != 1 || rows[3].Code != "service" || rows[4].RetryInMS != 19990 || rows[5].Attempts != 2 || rows[6].Attempts != 2 {
		t.Fatal(rows)
	}
}

func TestWANDiagnosticsRejectRawOrMalformedHelperData(t *testing.T) {
	for _, body := range []string{
		`"event":"diagnostic","kind":"relay_disconnected","code":"Bearer SECRET","attempts":0,"elapsedMs":0}`,
		`"event":"diagnostic","kind":"private https://SECRET.invalid","code":"network","attempts":0,"elapsedMs":0}`,
		`"event":"diagnostic","kind":"retry_failed","code":"network","cause":"SECRET","attempts":1,"elapsedMs":0}`,
		`"event":"diagnostic","kind":"retry_failed","code":"network","attempts":16,"elapsedMs":0}`,
		`"event":"diagnostic","kind":"retry_failed","code":"network","attempts":1,"elapsedMs":0.25}`,
		`"event":"diagnostic","kind":"retry_failed","code":"network","attempts":1,"elapsedMs":300001}`,
		`"event":"diagnostic","kind":"retry_failed","code":"network","attempts":1,"elapsedMs":null}`,
		`"event":"diagnostic","kind":"retry_failed","code":"network","attempts":1,"elapsedMs":1,"token":"SECRET"}`,
	} {
		t.Run(body, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wan-diagnostics.jsonl")
			d := newDiagnosticLog(path)
			out := testSDKOutput()
			out.phase = 3
			out.journal = d
			out.Write([]byte(`{"version":1,"generation":"` + out.generation + `",` + body + "\n"))
			if out.failed == nil {
				t.Fatal("invalid diagnostic accepted")
			}
			if err := d.close(); err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(path)
			if strings.Contains(string(raw), "SECRET") {
				t.Fatal("raw malformed data entered journal")
			}
			rows := readDiagnostics(t, path)
			if len(rows) != 1 || rows[0].Code != "protocol_output" || rows[0].Event != "helper_failed" {
				t.Fatal(rows)
			}
		})
	}
	code, reason := diagnosticFailure(errors.New("access_token=SECRET https://private.invalid"))
	if code != "unclassified" || strings.Contains(reason, "SECRET") {
		t.Fatal(code, reason)
	}
}

func TestWANDiagnosticsManagerPersistsFinalFailureAndStop(t *testing.T) {
	for _, mode := range []string{"failure", "ok"} {
		t.Run(mode, func(t *testing.T) {
			m, _ := fakeManager(t, mode, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			err := m.Start(context.Background())
			if mode == "failure" && err == nil {
				t.Fatal("auth failure did not fail")
			}
			if mode == "ok" && err != nil {
				t.Fatal(err)
			}
			m.Stop()
			rows := readDiagnostics(t, diagnosticPath(m.path))
			raw, _ := os.ReadFile(diagnosticPath(m.path))
			for _, secret := range []string{"fake-sdk-token", "SECRET-SHOULD-NEVER-LEAK", "actual-id", "devtunnels.ms"} {
				if strings.Contains(string(raw), secret) {
					t.Fatal("private helper data persisted", secret)
				}
			}
			if rows[0].Event != "starting" {
				t.Fatal(rows)
			}
			last := rows[len(rows)-1]
			if mode == "failure" {
				if last.Event != "failed" || last.Code != "auth" {
					t.Fatal(last)
				}
				found := false
				for _, r := range rows {
					if r.Event == "helper_failed" && r.Code == "auth" {
						found = true
					}
				}
				if !found {
					t.Fatal("helper failure reason was not persisted")
				}
			} else {
				if last.Event != "stopped" {
					t.Fatal(last)
				}
				for _, r := range rows {
					if r.Event == "helper_failed" || r.Event == "failed" {
						t.Fatal("normal Stop logged as failure", r)
					}
				}
			}
		})
	}
}

func TestWANDiagnosticsWriteFailureDoesNotFailWAN(t *testing.T) {
	m, _ := fakeManager(t, "ok", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if err := os.Mkdir(diagnosticPath(m.path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal("diagnostic failure stopped startup", err)
	}
	if m.Status().State != "running" {
		t.Fatal(m.Status())
	}
	m.Stop()
	if m.Status().State != "stopped" {
		t.Fatal(m.Status())
	}
}

func TestWANDiagnosticsSlowWriterDoesNotBlockRevocation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var rows []diagnosticRecord
	d := startDiagnosticLog(func(record diagnosticRecord) error {
		once.Do(func() { close(entered); <-release })
		rows = append(rows, record)
		return nil
	})
	d.record(diagnosticRecord{Event: "starting"})
	<-entered
	var admitted atomic.Bool
	admitted.Store(true)
	out := testSDKOutput()
	out.journal = d
	out.revoke = func() { admitted.Store(false) }
	for i := 0; i < 1000; i++ {
		d.record(diagnosticRecord{Event: "retry_started", Attempts: 1})
	}
	// The writer is held until after this returns: no timing threshold is needed.
	out.Write([]byte("SECRET-MALFORMED\n"))
	if admitted.Load() {
		t.Fatal("diagnostic writer prevented revocation")
	}
	close(release)
	if err := d.close(diagnosticRecord{Event: "failed", Code: "protocol_output"}); err != nil {
		t.Fatal(err)
	}
	if rows[len(rows)-1].Event != "failed" {
		t.Fatal("overflow lost the final outcome")
	}
	found := false
	for _, r := range rows {
		if r.Event == "events_dropped" && r.Dropped > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("bounded queue silently dropped overflow")
	}
	d.record(diagnosticRecord{Event: "after_close"})
}

func TestWANDiagnosticsPreserveTerminalSDKCategory(t *testing.T) {
	for _, code := range []string{"host", "protocol", "dispose", "reconnect_timeout"} {
		t.Run(code, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wan-diagnostics.jsonl")
			d := newDiagnosticLog(path)
			out := testSDKOutput()
			out.phase = 3
			out.journal = d
			writeSDKEvent(t, out, sdkEvent{Version: 1, Generation: out.generation, Event: "error", Code: code})
			if err := d.close(); err != nil {
				t.Fatal(err)
			}
			rows := readDiagnostics(t, path)
			if len(rows) != 1 || rows[0].Code != code || rows[0].Reason != sdkError(code).Error() {
				t.Fatal(rows)
			}
		})
	}
}
