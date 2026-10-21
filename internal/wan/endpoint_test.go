package wan

import "testing"

func TestEndpointBelongsToOwnedPort(t *testing.T) {
	for _, raw := range []string{
		"https://actual-id-8080.usw2.devtunnels.ms",
		"https://actual-id-8080.usw2.devtunnels.ms/",
		"https://actual-id.usw2.devtunnels.ms:8080/",
	} {
		if _, err := endpointURL(raw, 8080); err != nil {
			t.Errorf("refused endpoint %q: %v", raw, err)
		}
	}
	for _, raw := range []string{
		"", "https://", "http://actual-id-8080.usw2.devtunnels.ms",
		"https://actual-id-8081.usw2.devtunnels.ms",
		"https://actual-id.usw2.devtunnels.ms:8081/",
		"https://actual-id-8080-inspect.usw2.devtunnels.ms/",
		"https://actual-id-8080.usw2.devtunnels.ms.attacker.example/",
		"https://actual-id-8080.attacker.example/",
		"https://actual-id-8080.usw2.devtunnels.ms@attacker.example/",
		"https://secret@actual-id-8080.usw2.devtunnels.ms/",
		"https://actual-id-8080.usw2.devtunnels.ms/?token=secret",
		"https://actual-id-8080.usw2.devtunnels.ms/?",
		"https://actual-id-8080.usw2.devtunnels.ms/#inspection",
		"https://actual-id-8080.usw2.devtunnels.ms/v1",
		"https://actual-id-8080.usw2.devtunnels.ms/%2f",
		"https://actual-id-8080.usw2.devtunnels.ms/../",
		"https://-actual-id-8080.usw2.devtunnels.ms/",
		"https://actual-id-8080.us-w2.devtunnels.ms/",
	} {
		if _, err := endpointURL(raw, 8080); err == nil {
			t.Errorf("accepted endpoint outside the owned port: %q", raw)
		}
	}
}
