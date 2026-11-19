package wan

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// endpointURL accepts only a dev tunnels HTTPS endpoint for the owned port.
// Neither a relay's diagnostics nor its inspection page is an inference URL.
func endpointURL(raw string, portNumber int) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("unsupported WAN endpoint")
	}
	host := u.Hostname()
	labels := strings.Split(host, ".")
	port := strconv.Itoa(portNumber)
	if len(labels) != 4 || labels[2] != "devtunnels" || labels[3] != "ms" || !regionLabel(labels[1]) {
		return nil, errors.New("unsupported WAN endpoint host")
	}
	id := labels[0]
	if u.Port() == "" {
		if !strings.HasSuffix(id, "-"+port) {
			return nil, errors.New("WAN endpoint does not match the saved port")
		}
		id = strings.TrimSuffix(id, "-"+port)
	} else if u.Port() != port {
		return nil, errors.New("WAN endpoint does not match the saved port")
	}
	if len(id) == 0 || len(id) > 60 || strings.HasPrefix(id, "-") || strings.HasSuffix(id, "-") {
		return nil, errors.New("unsupported WAN endpoint id")
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return nil, errors.New("unsupported WAN endpoint id")
		}
	}
	return u, nil
}

func regionLabel(s string) bool {
	if s == "" || len(s) > 30 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
