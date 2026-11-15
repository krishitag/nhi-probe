package fingerprint

import "testing"

func TestFull(t *testing.T) {
	tests := []struct {
		name   string
		secret string
		want   string
	}{
		// Known SHA-256 value for "abc" (from the official SHA-256 test vectors).
		{"known value", "abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{"empty string", "", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Full(tt.secret); got != tt.want {
				t.Errorf("Full(%q) = %q, want %q", tt.secret, got, tt.want)
			}
		})
	}
}

func TestFullSameInputSameOutput(t *testing.T) {
	if Full("same") != Full("same") {
		t.Error("same secret gave different fingerprints")
	}
	if Full("one") == Full("two") {
		t.Error("different secrets gave the same fingerprint")
	}
}

func TestShort(t *testing.T) {
	tests := []struct {
		name string
		full string
		want string
	}{
		{"cuts to 12", Full("abc"), "ba7816bf8f01"},
		{"already short", "abc", "abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Short(tt.full); got != tt.want {
				t.Errorf("Short(%q) = %q, want %q", tt.full, got, tt.want)
			}
		})
	}
}

func TestRedact(t *testing.T) {
	tests := []struct {
		name   string
		secret string
		want   string
	}{
		{"normal secret", "abcdefghijklmnop", "abcd...mnop"},
		{"exactly 12", "abcdefghijkl", "abcd...ijkl"},
		{"too short", "abcdefgh", "****"},
		{"empty", "", "****"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Redact(tt.secret); got != tt.want {
				t.Errorf("Redact(%q) = %q, want %q", tt.secret, got, tt.want)
			}
		})
	}
}
