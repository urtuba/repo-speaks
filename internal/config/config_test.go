package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want Config
	}{
		{"defaults", nil, Config{Addr: ":8080", DataDir: "data"}},
		{"from environment", map[string]string{
			"REPO_SPEAKS_ADDR":              "127.0.0.1:9000",
			"REPO_SPEAKS_DATA_DIR":          "/data",
			"REPO_SPEAKS_ALLOW_PRIVATE_LLM": "true",
		}, Config{Addr: "127.0.0.1:9000", DataDir: "/data", AllowPrivateLLM: true}},
		{"private LLM off", map[string]string{"REPO_SPEAKS_ALLOW_PRIVATE_LLM": "false"},
			Config{Addr: ":8080", DataDir: "data"}},
		{"private LLM, unclear value", map[string]string{"REPO_SPEAKS_ALLOW_PRIVATE_LLM": "yes"},
			Config{Addr: ":8080", DataDir: "data"}},
		{"backups", map[string]string{
			"REPO_SPEAKS_BACKUP_DIR":                  "/backups",
			"REPO_SPEAKS_BACKUP_S3_ENDPOINT":          "https://s3.example.com",
			"REPO_SPEAKS_BACKUP_S3_BUCKET":            "team",
			"REPO_SPEAKS_BACKUP_S3_REGION":            "eu-central-1",
			"REPO_SPEAKS_BACKUP_S3_PREFIX":            "/repo-speaks/",
			"REPO_SPEAKS_BACKUP_S3_ACCESS_KEY_ID":     "key-id",
			"REPO_SPEAKS_BACKUP_S3_SECRET_ACCESS_KEY": "secret-key",
			"REPO_SPEAKS_BACKUP_PASSPHRASE":           "a long passphrase here",
		}, Config{Addr: ":8080", DataDir: "data", Backup: Backup{
			Dir: "/backups",
			S3: S3{
				Endpoint: "https://s3.example.com", Bucket: "team", Region: "eu-central-1", Prefix: "repo-speaks",
				AccessKeyID: "key-id", SecretAccessKey: "secret-key",
			},
			Passphrase: "a long passphrase here",
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(key string) string { return tt.env[key] }
			if got := Load(getenv); got != tt.want {
				t.Errorf("Load() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// Secret values used by the tests. None may appear in an error.
const (
	testPassphrase = "correct horse battery staple"
	testKeyID      = "AKIDEXAMPLE1234"
	testSecretKey  = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

// fullS3 sets every S3 setting that must be set.
func fullS3() S3 {
	return S3{Endpoint: "https://s3.example.com", Bucket: "team", AccessKeyID: testKeyID, SecretAccessKey: testSecretKey}
}

func TestCheck(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	outside := filepath.Join(root, "backups")
	for _, dir := range []string{data, outside, filepath.Join(data, "backups")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	// A link outside the data folder that points into it.
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(data, "backups"), link); err != nil {
		t.Fatal(err)
	}
	// The data folder itself reached through a link.
	dataLink := filepath.Join(root, "data-link")
	if err := os.Symlink(data, dataLink); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		dataDir string
		backup  Backup
		want    string // part of the error, or "" for none
	}{
		{"backups off", data, Backup{}, ""},
		{"passphrase alone, for a restore", data, Backup{Passphrase: testPassphrase}, ""},
		{"folder", data, Backup{Dir: outside, Passphrase: testPassphrase}, ""},
		{"folder that does not exist yet", data, Backup{Dir: filepath.Join(outside, "new"), Passphrase: testPassphrase}, ""},
		{"S3", data, Backup{S3: fullS3(), Passphrase: testPassphrase}, ""},
		{"S3 over plain HTTP", data, Backup{S3: S3{Endpoint: "http://minio:9000", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s"}, Passphrase: testPassphrase}, ""},
		{"both", data, Backup{Dir: outside, S3: fullS3(), Passphrase: testPassphrase}, ""},

		{"folder without passphrase", data, Backup{Dir: outside}, "REPO_SPEAKS_BACKUP_PASSPHRASE is empty"},
		{"S3 without passphrase", data, Backup{S3: fullS3()}, "REPO_SPEAKS_BACKUP_PASSPHRASE is empty"},
		{"short passphrase", data, Backup{Dir: outside, Passphrase: "fifteen chars.."}, "too short"},
		{"short passphrase without target", data, Backup{Passphrase: "short"}, "too short"},
		{"half S3", data, Backup{S3: S3{Endpoint: "https://s3.example.com", AccessKeyID: testKeyID}, Passphrase: testPassphrase},
			"REPO_SPEAKS_BACKUP_S3_BUCKET, REPO_SPEAKS_BACKUP_S3_SECRET_ACCESS_KEY missing"},
		{"prefix alone", data, Backup{S3: S3{Prefix: "repo-speaks"}, Passphrase: testPassphrase}, "S3 backups are half set"},
		{"endpoint without scheme", data, Backup{S3: S3{Endpoint: "s3.example.com", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s"}, Passphrase: testPassphrase}, "must be an address"},
		{"endpoint with a path", data, Backup{S3: S3{Endpoint: "https://s3.example.com/team", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s"}, Passphrase: testPassphrase}, "must be an address"},
		{"folder is the data folder", data, Backup{Dir: data, Passphrase: testPassphrase}, "inside the data folder"},
		{"folder inside the data folder", data, Backup{Dir: filepath.Join(data, "backups"), Passphrase: testPassphrase}, "inside the data folder"},
		{"folder inside, written with ..", data, Backup{Dir: filepath.Join(outside, "..", "data", "backups"), Passphrase: testPassphrase}, "inside the data folder"},
		{"link into the data folder", data, Backup{Dir: link, Passphrase: testPassphrase}, "inside the data folder"},
		{"data folder through a link", dataLink, Backup{Dir: filepath.Join(data, "backups"), Passphrase: testPassphrase}, "inside the data folder"},
		{"new folder inside the data folder", data, Backup{Dir: filepath.Join(link, "new"), Passphrase: testPassphrase}, "inside the data folder"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Config{DataDir: tt.dataDir, Backup: tt.backup}.Check()
			if tt.want == "" {
				if err != nil {
					t.Errorf("Check() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Check() = %v, want it to contain %q", err, tt.want)
			}
			for _, secret := range []string{testPassphrase, testKeyID, testSecretKey, "fifteen chars"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error shows a secret: %v", err)
				}
			}
		})
	}
}

func TestCheckRelativePaths(t *testing.T) {
	t.Chdir(t.TempDir())
	err := Config{DataDir: "data", Backup: Backup{Dir: "data/backups", Passphrase: testPassphrase}}.Check()
	if err == nil || !strings.Contains(err.Error(), "inside the data folder") {
		t.Errorf("Check() = %v, want inside the data folder", err)
	}
	if err := (Config{DataDir: "data", Backup: Backup{Dir: "data-backups", Passphrase: testPassphrase}}).Check(); err != nil {
		t.Errorf("sibling folder with a longer name: Check() = %v, want nil", err)
	}
}

func TestSecretHidden(t *testing.T) {
	c := Config{Backup: Backup{S3: fullS3(), Passphrase: testPassphrase}}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	logger.Info("config", "passphrase", c.Backup.Passphrase, "s3", c.Backup.S3)
	printed := fmt.Sprintf("%v %+v %#v %s", c, c, c, c.Backup.Passphrase) + logs.String()
	for _, secret := range []string{testPassphrase, testKeyID, testSecretKey} {
		if strings.Contains(printed, secret) {
			t.Errorf("printed config shows %q:\n%s", secret, printed)
		}
	}
	if string(c.Backup.Passphrase) != testPassphrase {
		t.Error("string() does not give the value")
	}
}
