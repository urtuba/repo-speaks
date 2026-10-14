// Package config reads Repo Speaks' settings from the environment.
package config

import "strconv"

// Config holds the settings the server needs to start.
type Config struct {
	Addr    string // address the web server listens on
	DataDir string // folder for the database and repository clones
	// AllowPrivateLLM lets models use hosts on private addresses, such
	// as a local Ollama. It is off by default: a project admin sets a
	// model's URL, and could make the server call internal systems.
	AllowPrivateLLM bool
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
	// Anything strconv.ParseBool does not read as true leaves it off.
	c.AllowPrivateLLM, _ = strconv.ParseBool(getenv("REPO_SPEAKS_ALLOW_PRIVATE_LLM"))
	return c
}
