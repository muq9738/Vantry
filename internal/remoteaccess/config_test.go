package remoteaccess

import "testing"

func TestRelayAddressRulesAndPairValidation(t *testing.T) {
	for _, value := range []string{"https://relay.example.com", "http://127.0.0.1:8787", "http://localhost:8787"} {
		if _, err := ValidateRelay(value); err != nil {
			t.Fatalf("valid relay rejected: %v", err)
		}
	}
	if _, err := ValidateRelay("http://192.168.31.118:8787"); err == nil {
		t.Fatal("private plaintext HTTP relay accepted without opt-in")
	}
	if _, err := ValidateRelayWithOptions("http://192.168.31.118:8787", true); err != nil {
		t.Fatalf("private plaintext HTTP relay rejected with opt-in: %v", err)
	}
	for _, value := range []string{"http://public.example.com", "https://user@relay.example.com", "https://relay.example.com?secret=x", "ssh://mini"} {
		if _, err := ValidateRelay(value); err == nil {
			t.Fatal("invalid relay accepted")
		}
		if _, err := ValidateRelayWithOptions(value, true); err == nil {
			t.Fatal("invalid relay accepted even with private HTTP opt-in")
		}
	}
	if _, err := ParseLink("other-app://pair#unknown"); err == nil {
		t.Fatal("foreign application link was incorrectly accepted as a Vantry pairing")
	}
	pair, _, err := newIdentity("Mac", "https://relay.example.com")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseLink(pair.Link())
	if err != nil || parsed.Name != "Mac" {
		t.Fatalf("pair link failed: %v", err)
	}
}
