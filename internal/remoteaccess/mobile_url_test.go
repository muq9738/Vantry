package remoteaccess

import "testing"

func TestMobilePublicURLCannotPublishLoopbackToAPhone(t *testing.T) {
	for _, value := range []string{"http://127.0.0.1:13773", "https://127.0.0.1:13773", "https://localhost", "https://localhost.", "https://[::1]", "https://0.0.0.0", "https://phone.localhost", "https://example.com/subpath"} {
		t.Run(value, func(t *testing.T) {
			if _, err := mobilePublicURL(value); err == nil {
				t.Fatal("unusable mobile address accepted")
			}
		})
	}
	if got, err := mobilePublicURL("https://computer.example.com:9443/"); err != nil || got != "https://computer.example.com:9443" {
		t.Fatalf("reachable HTTPS address rejected: %q %v", got, err)
	}
}
