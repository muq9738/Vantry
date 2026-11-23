package desktop

import "testing"

func TestMobilePairingPresentationMatchesQRCode(t *testing.T) {
	code, address, ok := mobilePairingPresentation("https://computer.example:9443/#token=SESSION-MOBILE-CODE-ONLY")
	if !ok || code != "SESSION-MOBILE-CODE-ONLY" || address != "https://computer.example:9443" {
		t.Fatal("the manual address and code must match the mobile QR payload")
	}
	for _, link := range []string{"http://127.0.0.1:13773/#token=fixture", "https://localhost/#token=fixture", "https://computer.example/", "https://computer.example/subpath#token=fixture"} {
		if _, _, ok := mobilePairingPresentation(link); ok {
			t.Fatal("unusable mobile QR code accepted")
		}
	}
}
