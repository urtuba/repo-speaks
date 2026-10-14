package llm

import (
	"strings"
	"testing"
)

func TestParseBaseURL(t *testing.T) {
	tests := []struct {
		in   string
		want string // "" when it must be refused
	}{
		{DefaultBaseURL, DefaultBaseURL},
		{"  https://openrouter.ai/api/v1/  ", DefaultBaseURL},
		{"https://openrouter.ai/api/v1//", DefaultBaseURL},
		{"HTTPS://api.example.com/v1", "https://api.example.com/v1"},
		{"http://localhost:11434/v1", "http://localhost:11434/v1"},
		{"http://127.0.0.1:11434/v1", "http://127.0.0.1:11434/v1"},
		{"https://api.example.com", "https://api.example.com"},
		{"https://api.example.com/", "https://api.example.com"},
		{"https://[2001:db8::1]:8443/v1", "https://[2001:db8::1]:8443/v1"},
		{"https://api.example.com/a%2Fb/", "https://api.example.com/a%2Fb"},

		{"", ""},
		{"   ", ""},
		{"openrouter.ai/api/v1", ""},
		{"/api/v1", ""},
		{"//openrouter.ai/api/v1", ""},
		{"ftp://example.com/v1", ""},
		{"file:///etc/passwd", ""},
		{"javascript:alert(1)", ""},
		{"mailto:a@example.com", ""},
		{"https://", ""},
		{"https:///v1", ""},
		{"https://user:pass@api.example.com/v1", ""},
		{"https://user@api.example.com/v1", ""},
		{"https://api.example.com/v1?key=x", ""},
		{"https://api.example.com/v1?", ""},
		{"https://api.example.com/v1#x", ""},
		{"https://api.example.com/v1#", ""},
		{"https://api.example.com/v1 x", ""},
		{"https://api.exa mple.com/v1", ""},
		{"https://" + strings.Repeat("a", 500) + ".com", ""},
	}
	for _, tt := range tests {
		got, err := ParseBaseURL(tt.in)
		if tt.want == "" {
			if err == nil {
				t.Errorf("ParseBaseURL(%q) = %q, want an error", tt.in, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("ParseBaseURL(%q) = %q, %v, want %q", tt.in, got, err, tt.want)
		}
	}
}
