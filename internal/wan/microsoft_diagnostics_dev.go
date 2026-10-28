//go:build dev

package wan

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"log"
	"strings"
	"time"
)

// Development diagnostics deliberately exclude URLs, user identifiers, OAuth
// codes, vendor descriptions and token contents. Only schema metadata is logged.
func microsoftResponseDiagnostic(status int, body []byte) {
	var result struct {
		githubGrant
		Codes []int `json:"error_codes"`
	}
	if json.Unmarshal(body, &result) != nil {
		log.Printf("WAN Microsoft OAuth: http=%d readable=false", status)
		return
	}
	codes := make([]int, 0, 8)
	for _, code := range result.Codes {
		if len(codes) < 8 && code > 0 && code <= 9999999 {
			codes = append(codes, code)
		}
	}
	_, identity := microsoftGrantAccount(result.githubGrant)
	log.Printf("WAN Microsoft OAuth: http=%d error=%t error_codes=%v access_bytes=%d refresh_bytes=%d bearer=%t expires_in=%d client_info_bytes=%d identity_valid=%t id_token_bytes=%d",
		status, result.Error != "", codes, len(result.Access), len(result.Refresh), strings.EqualFold(result.Type, "bearer"), result.ExpiresIn, len(result.ClientInfo), identity, len(result.IDToken))
	if result.Error == "" && validMicrosoftGrant(result.githubGrant) && identity {
		data, err := json.Marshal(microsoftCredential(result.githubGrant, time.Now()))
		if err == nil {
			defer clear(data)
			var compressed bytes.Buffer
			compressed.WriteString("magpie-wan-z1\n")
			w := zlib.NewWriter(&compressed)
			_, _ = w.Write(data)
			_ = w.Close()
			log.Printf("WAN Microsoft OAuth: secure_record_bytes=%d compressed_bytes=%d single_blob_limit=%d fits_single_blob=%t", len(data), compressed.Len(), maxCredentialBlob, compressed.Len() <= maxCredentialBlob)
			clear(compressed.Bytes())
		}
	}
}

func microsoftBrowserDiagnostic(stage string) {
	log.Printf("WAN Microsoft browser: %s", stage)
}
