//go:build !darwin

package app

func accessibilityTrusted() bool { return false }

func registerAccessibility() (bool, error) { return false, nil }
