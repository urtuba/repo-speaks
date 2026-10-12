// Package config reads Repo Speaks' settings from the environment.
package config

// Config holds the settings the server needs to start.
type Config struct {
	Addr    string // address the web server listens on
	DataDir string // folder for the database and repository clones
}

// Load reads settings through getenv and fills in defaults.
// Taking getenv as a parameter keeps tests free of real environment variables.
func Load(getenv func(string) string) Config {
	c := Config{
		Addr:    ":8080",
		DataDir: "data",
	}
	if v := getenv("REPO_SPEAKS_ADDR"); v != "" {
		c.Addr = v
	}
	if v := getenv("REPO_SPEAKS_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	return c
}
