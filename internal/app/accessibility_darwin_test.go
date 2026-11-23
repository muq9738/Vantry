//go:build darwin

package app

import "testing"

// Loading the framework symbols must succeed; reading trust never prompts.
func TestAccessibilityBindingLoads(t *testing.T) {
	if err := loadAccessibility(); err != nil {
		t.Fatal(err)
	}
	if accessibilityAPI.promptKey == 0 || accessibilityAPI.trueValue == 0 {
		t.Fatal("CF globals unresolved")
	}
	_ = accessibilityTrusted()
}
