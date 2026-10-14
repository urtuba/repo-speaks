// Package config reads Repo Speaks' settings from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Config holds the settings the server needs to start.
type Config struct {
	Addr    string // address the web server listens on
	DataDir string // folder for the database and repository clones
	// AllowPrivateLLM lets models use hosts on private addresses, such
	// as a local Ollama. It is off by default: a project admin sets a
	// model's URL, and could make the server call internal systems.
	AllowPrivateLLM bool
	Backup          Backup
}

// Backup holds the backup settings. With no target set, backups are
// off. They live in the environment and never in the database, so a
// backup never holds the keys to its own storage.
type Backup struct {
	Dir        string // a mounted folder, such as an external disk
	S3         S3
	Passphrase Secret // encrypts every backup
}

// S3 is an S3-compatible bucket for backups.
type S3 struct {
	Endpoint        string // such as https://s3.eu-central-1.amazonaws.com
	Bucket          string
	Region          string // optional for most hosts
	Prefix          string // optional folder in the bucket, such as repo-speaks
	AccessKeyID     Secret
	SecretAccessKey Secret
}

// Secret is a setting that must never reach a log, an error or a page.
// Printing it shows a mark instead of the value; string(s) reads it.
type Secret string

func (Secret) String() string                 { return "[hidden]" }
func (Secret) GoString() string               { return `"[hidden]"` }
func (Secret) LogValue() slog.Value           { return slog.StringValue("[hidden]") }
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// minPassphrase is the shortest backup passphrase allowed, in
// characters. A lost passphrase means a lost backup, and a short one
// can be guessed by anyone who gets a backup file.
const minPassphrase = 16

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
	c.Backup = Backup{
		Dir: getenv("REPO_SPEAKS_BACKUP_DIR"),
		S3: S3{
			Endpoint:        getenv("REPO_SPEAKS_BACKUP_S3_ENDPOINT"),
			Bucket:          getenv("REPO_SPEAKS_BACKUP_S3_BUCKET"),
			Region:          getenv("REPO_SPEAKS_BACKUP_S3_REGION"),
			Prefix:          strings.Trim(getenv("REPO_SPEAKS_BACKUP_S3_PREFIX"), "/"),
			AccessKeyID:     Secret(getenv("REPO_SPEAKS_BACKUP_S3_ACCESS_KEY_ID")),
			SecretAccessKey: Secret(getenv("REPO_SPEAKS_BACKUP_S3_SECRET_ACCESS_KEY")),
		},
		Passphrase: Secret(getenv("REPO_SPEAKS_BACKUP_PASSPHRASE")),
	}
	return c
}

// On reports whether a backup target is set.
func (b Backup) On() bool {
	return b.Dir != "" || b.S3.Set()
}

// Set reports whether any S3 setting is set.
func (s S3) Set() bool {
	return s != S3{}
}

// Check reports settings that cannot work, such as a backup target
// without a passphrase. The server and the commands refuse to start
// with them. Messages name the settings, never their values.
func (c Config) Check() error {
	var errs []error
	b := c.Backup
	if b.On() && b.Passphrase == "" {
		errs = append(errs, errors.New("a backup target is set, but REPO_SPEAKS_BACKUP_PASSPHRASE is empty: every backup is encrypted with it"))
	}
	if b.Passphrase != "" && utf8.RuneCountInString(string(b.Passphrase)) < minPassphrase {
		errs = append(errs, fmt.Errorf("REPO_SPEAKS_BACKUP_PASSPHRASE is too short: use at least %d characters", minPassphrase))
	}
	if b.S3.Set() {
		errs = append(errs, b.S3.check()...)
	}
	if b.Dir != "" {
		if err := checkBackupDir(b.Dir, c.DataDir); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// check reports S3 settings that are missing or wrong.
func (s S3) check() []error {
	var missing []string
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"REPO_SPEAKS_BACKUP_S3_ENDPOINT", s.Endpoint != ""},
		{"REPO_SPEAKS_BACKUP_S3_BUCKET", s.Bucket != ""},
		{"REPO_SPEAKS_BACKUP_S3_ACCESS_KEY_ID", s.AccessKeyID != ""},
		{"REPO_SPEAKS_BACKUP_S3_SECRET_ACCESS_KEY", s.SecretAccessKey != ""},
	} {
		if !f.set {
			missing = append(missing, f.name)
		}
	}
	var errs []error
	if len(missing) > 0 {
		errs = append(errs, fmt.Errorf("S3 backups are half set: %s missing", strings.Join(missing, ", ")))
	}
	if s.Endpoint != "" {
		if _, _, err := s.EndpointHost(); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// EndpointHost splits the endpoint into its host, with the port if it
// has one, and whether it uses HTTPS.
func (s S3) EndpointHost() (host string, secure bool, err error) {
	u, err := url.Parse(s.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
		u.User != nil || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", false, errors.New("REPO_SPEAKS_BACKUP_S3_ENDPOINT must be an address such as https://s3.example.com, with no path")
	}
	return u.Host, u.Scheme == "https", nil
}

// checkBackupDir refuses a backup folder inside the data folder: a
// backup on the same disk as the data dies with it. Both paths are made
// absolute and their links are followed, so a link cannot hide it.
func checkBackupDir(backupDir, dataDir string) error {
	backup, err := realPath(backupDir)
	if err != nil {
		return fmt.Errorf("REPO_SPEAKS_BACKUP_DIR: %w", err)
	}
	data, err := realPath(dataDir)
	if err != nil {
		return fmt.Errorf("REPO_SPEAKS_DATA_DIR: %w", err)
	}
	if rel, err := filepath.Rel(data, backup); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("REPO_SPEAKS_BACKUP_DIR is inside the data folder: a backup must live on another disk, such as a mounted external disk or NAS")
	}
	return nil
}

// realPath makes path absolute and follows its links. A part of the
// path that does not exist yet is kept as written, since the data
// folder is made at start.
func realPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var rest []string
	for p := abs; ; p = filepath.Dir(p) {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(append([]string{real}, rest...)...), nil
		}
		if !os.IsNotExist(err) || filepath.Dir(p) == p {
			return "", err
		}
		rest = append([]string{filepath.Base(p)}, rest...)
	}
}
