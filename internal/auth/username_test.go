package auth

import (
	"strings"
	"testing"
)

func TestValidateUsername(t *testing.T) {
	tests := []struct {
		username string
		wantErr  bool
	}{
		{"samed", false},
		{"a-b_c9", false},
		{"abc", false},
		{strings.Repeat("a", 32), false},
		{"ab", true},
		{strings.Repeat("a", 33), true},
		{"Samed", true},
		{"sam ed", true},
		{"samed@example", true},
		{"şamed", true},
		{"", true},
	}
	for _, tt := range tests {
		t.Run(tt.username, func(t *testing.T) {
			if err := ValidateUsername(tt.username); (err != nil) != tt.wantErr {
				t.Errorf("ValidateUsername(%q) = %v, want error %v", tt.username, err, tt.wantErr)
			}
		})
	}
}
