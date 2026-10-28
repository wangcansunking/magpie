//go:build dev

package wan

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"
)

func TestWANMicrosoftDiagnosticsNeverLogCredentials(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	g := githubGrant{Access: "SECRET-access", Refresh: "SECRET-refresh", Type: "Bearer", ExpiresIn: 3600, ClientInfo: fixtureMicrosoftInfo(), IDToken: "SECRET-id-token"}
	body, _ := json.Marshal(g)
	microsoftResponseDiagnostic(200, body)
	microsoftResponseDiagnostic(400, []byte(`{"error":"SECRET-error","error_description":"SECRET-description","error_codes":[53003,-1,10000000]}`))
	microsoftResponseDiagnostic(200, []byte(`SECRET-malformed`))
	text := output.String()
	if strings.Contains(text, "SECRET") || strings.Contains(text, g.ClientInfo) || !strings.Contains(text, "error_codes=[53003]") || !strings.Contains(text, "fits_single_blob=") {
		t.Fatal("development diagnostics exposed credentials or lost safe metadata")
	}
}
