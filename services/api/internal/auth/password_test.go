package auth

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidatePassword(t *testing.T) {
	for _, tt := range []struct {
		pw string
		ok bool
	}{
		{"short", false},
		{strings.Repeat("a", MinPasswordLen-1), false},
		{strings.Repeat("a", MinPasswordLen), true},
		{strings.Repeat("ü", MinPasswordLen), true}, // characters, not bytes, for the minimum
		{strings.Repeat("a", MaxPasswordLen), true},
		{strings.Repeat("a", MaxPasswordLen+1), false},
	} {
		if err := ValidatePassword(tt.pw); (err == nil) != tt.ok {
			t.Errorf("ValidatePassword(len %d) = %v, want ok=%v", len(tt.pw), err, tt.ok)
		}
	}
}

func TestCheckPasswordEmptyHash(t *testing.T) {
	if err := CheckPassword("", "anything at all"); err != ErrBadPassword {
		t.Errorf("empty hash: %v", err)
	}
	if err := CheckPassword("not-a-bcrypt-hash", "anything at all"); err != ErrBadPassword {
		t.Errorf("garbage hash: %v", err)
	}
}

func TestClientIP(t *testing.T) {
	for _, tt := range []struct {
		name, header, value, want string
	}{
		{name: "peer", want: "192.0.2.1"},
		{name: "header unset in config", value: "203.0.113.5", want: "192.0.2.1"},
		{name: "header", header: "CF-Connecting-IP", value: "203.0.113.5", want: "203.0.113.5"},
		{name: "header list", header: "X-Forwarded-For", value: "203.0.113.5, 10.0.0.1", want: "203.0.113.5"},
		{name: "header empty", header: "CF-Connecting-IP", want: "192.0.2.1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/", nil)
			if tt.value != "" {
				r.Header.Set("CF-Connecting-IP", tt.value)
				r.Header.Set("X-Forwarded-For", tt.value)
			}
			if got := ClientIP(r, tt.header); got != tt.want {
				t.Errorf("ClientIP = %q, want %q", got, tt.want)
			}
		})
	}
}
