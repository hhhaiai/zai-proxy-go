package internal

import "testing"

func TestGenerateSignature(t *testing.T) {
	sig := GenerateSignature("user1", "req1", "hello", 1000000)
	if sig == "" {
		t.Error("expected non-empty signature")
	}
	// Same inputs should produce same signature
	sig2 := GenerateSignature("user1", "req1", "hello", 1000000)
	if sig != sig2 {
		t.Error("same inputs should produce same signature")
	}
	// Different inputs should produce different signature
	sig3 := GenerateSignature("user2", "req1", "hello", 1000000)
	if sig == sig3 {
		t.Error("different user should produce different signature")
	}
}
