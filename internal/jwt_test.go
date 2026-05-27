package internal

import "testing"

func TestDecodeJWTPayload(t *testing.T) {
	// Valid JWT-like token (base64url encoded {"id":"user123"})
	// header: eyJhbGciOiJIUzI1NiJ9
	// payload: eyJpZCI6InVzZXIxMjMifQ
	// signature: sig
	token := "eyJhbGciOiJIUzI1NiJ9.eyJpZCI6InVzZXIxMjMifQ.sig"
	payload, err := DecodeJWTPayload(token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload == nil {
		t.Fatal("expected non-nil payload")
	}
	if payload.ID != "user123" {
		t.Errorf("ID = %q, want %q", payload.ID, "user123")
	}
}

func TestDecodeJWTPayload_Invalid(t *testing.T) {
	// Not enough parts
	payload, err := DecodeJWTPayload("not-a-jwt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload != nil {
		t.Errorf("expected nil payload for invalid token, got %v", payload)
	}
}
