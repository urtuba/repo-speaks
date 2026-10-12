package config

import "testing"

func TestLoad(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want Config
	}{
		{"defaults", nil, Config{Addr: ":8080", DataDir: "data"}},
		{"from environment", map[string]string{
			"REPO_SPEAKS_ADDR":     "127.0.0.1:9000",
			"REPO_SPEAKS_DATA_DIR": "/data",
		}, Config{Addr: "127.0.0.1:9000", DataDir: "/data"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(key string) string { return tt.env[key] }
			if got := Load(getenv); got != tt.want {
				t.Errorf("Load() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
