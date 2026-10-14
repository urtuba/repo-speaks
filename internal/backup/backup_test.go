package backup

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urtuba/repo-speaks/internal/config"
	"github.com/urtuba/repo-speaks/internal/store"
)

var testNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// Secret values used by the tests. None may appear in an error, a
// record or a target's description.
const (
	testPassphrase = "correct horse battery staple"
	testKeyID      = "AKIDEXAMPLE1234"
	testSecretKey  = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	testAPIKey     = "sk-or-v1-model-key-in-the-database"
)

// fixture is a data folder whose database holds an owner and a model
// with an API key, and a backup folder outside it.
type fixture struct {
	dataDir   string
	dbPath    string
	backupDir string
	store     *store.Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := &fixture{dataDir: t.TempDir(), backupDir: t.TempDir()}
	f.dbPath = filepath.Join(f.dataDir, "repo-speaks.db")
	st, err := store.Open(ctx, f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f.store = st
	if _, err := st.CreateOwner(ctx, "samed", "hash", testNow); err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProject(ctx, "Customer portal", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddModel(ctx, store.NewModel{
		ProjectID: p.ID, Name: "Fast", BaseURL: "https://openrouter.ai/api/v1", ModelID: "z-ai/glm-5.3-flash", APIKey: testAPIKey,
	}, testNow); err != nil {
		t.Fatal(err)
	}
	return f
}

// service returns a backup service for the settings, with a fixed clock.
func (f *fixture) service(t *testing.T, cfg config.Backup) *Service {
	t.Helper()
	if cfg.Passphrase == "" {
		cfg.Passphrase = testPassphrase
	}
	s, err := New(f.store, f.dataDir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return testNow }
	return s
}

// checkNoTempFiles fails when the data folder holds anything but the
// database and its own files.
func (f *fixture) checkNoTempFiles(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "repo-speaks.db") {
			t.Errorf("data folder holds %s", e.Name())
		}
	}
}

// checkBackupFile decrypts a backup and checks that it holds the
// fixture's data.
func checkBackupFile(t *testing.T, encrypted []byte) {
	t.Helper()
	ctx := context.Background()
	if bytes.Contains(encrypted, []byte(testAPIKey)) || bytes.Contains(encrypted, []byte("SQLite format")) {
		t.Fatal("backup is not encrypted")
	}
	plainPath := filepath.Join(t.TempDir(), "plain.db")
	var plain bytes.Buffer
	if err := decrypt(ctx, &plain, bytes.NewReader(encrypted), []byte(testPassphrase)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plainPath, plain.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	checkDatabase(t, plainPath)
}

// checkDatabase checks that the database at path passes the integrity
// check and holds the fixture's owner and model key.
func checkDatabase(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.CheckFile(ctx, path); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.UserByUsername(ctx, "samed"); err != nil {
		t.Errorf("no owner in the backup: %v", err)
	}
	models, err := st.Models(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].Name != "Fast" {
		t.Errorf("models in the backup = %+v, want Fast", models)
	}
}

func TestFileName(t *testing.T) {
	got := FileName(time.Date(2026, 10, 14, 14, 5, 9, 0, time.FixedZone("TRT", 3*3600)), 8)
	if want := "repo-speaks-20261014T110509Z-schema0008.db.gpg"; got != want {
		t.Errorf("FileName() = %q, want %q", got, want)
	}
}

func TestBackupToFolder(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	s := f.service(t, config.Backup{Dir: f.backupDir})

	b, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	name := FileName(testNow, store.SchemaVersion())
	if b.Name != name || b.Targets != "folder "+f.backupDir || b.Error != "" || b.Size == 0 {
		t.Errorf("Run() = %+v", b)
	}
	path := filepath.Join(f.backupDir, name)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %v, want 0600", mode)
	}
	if info.Size() != b.Size {
		t.Errorf("file size %d, recorded %d", info.Size(), b.Size)
	}
	entries, _ := os.ReadDir(f.backupDir)
	if len(entries) != 1 {
		t.Errorf("backup folder holds %d files, want 1", len(entries))
	}
	encrypted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	checkBackupFile(t, encrypted)
	f.checkNoTempFiles(t)

	list, err := f.store.Backups(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != name || list[0].Error != "" {
		t.Errorf("records = %+v, want one good backup", list)
	}

	// A second backup in the same second never writes over the first.
	if _, err := s.Run(ctx); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second Run() in the same second = %v, want already exists", err)
	}
	f.checkNoTempFiles(t)
}

func TestBackupFailureLeavesNoTempFiles(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	missing := filepath.Join(f.backupDir, "not-mounted")
	s := f.service(t, config.Backup{Dir: missing})

	b, err := s.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "is the disk mounted") {
		t.Errorf("Run() error = %v, want a missing folder", err)
	}
	if b.Targets != "" || b.Size != 0 {
		t.Errorf("Run() = %+v, want no targets", b)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Error("the backup made the missing folder")
	}
	f.checkNoTempFiles(t)
	list, _ := f.store.Backups(ctx, 10)
	if len(list) != 1 || list[0].Error == "" {
		t.Errorf("records = %+v, want one failed backup", list)
	}

	// A copy that fails leaves nothing behind either.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.service(t, config.Backup{Dir: f.backupDir}).Run(cctx); err == nil {
		t.Error("Run() with an ended context: no error")
	}
	f.checkNoTempFiles(t)
	if entries, _ := os.ReadDir(f.backupDir); len(entries) != 0 {
		t.Errorf("backup folder holds %d files after a failed backup", len(entries))
	}
}

func TestBackupOff(t *testing.T) {
	f := newFixture(t)
	s, err := New(f.store, f.dataDir, config.Backup{})
	if err != nil {
		t.Fatal(err)
	}
	if s.On() {
		t.Error("On() = true with no target")
	}
	if _, err := s.Run(context.Background()); !errors.Is(err, ErrOff) {
		t.Errorf("Run() = %v, want ErrOff", err)
	}
}

func TestWrongPassphrase(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.service(t, config.Backup{Dir: f.backupDir}).Run(ctx); err != nil {
		t.Fatal(err)
	}
	encrypted, err := os.ReadFile(filepath.Join(f.backupDir, FileName(testNow, store.SchemaVersion())))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := decrypt(ctx, &out, bytes.NewReader(encrypted), []byte("another long passphrase")); !errors.Is(err, ErrPassphrase) {
		t.Errorf("decrypt() = %v, want ErrPassphrase", err)
	}
	if err := decrypt(ctx, &out, strings.NewReader("SQLite format 3\x00 not encrypted"), []byte(testPassphrase)); err == nil {
		t.Error("decrypt(plain file): no error")
	}
	// A changed byte fails the integrity check.
	damaged := bytes.Clone(encrypted)
	damaged[len(damaged)-30] ^= 0xff
	if err := decrypt(ctx, io.Discard, bytes.NewReader(damaged), []byte(testPassphrase)); err == nil {
		t.Error("decrypt(changed file): no error")
	}
}

// fakeS3 accepts uploads like an S3 host with path-style addresses.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte // by bucket/key
	auth    []string          // the Authorization header of each upload
	fail    string            // when set, every upload fails with this message
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Query().Has("location"):
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`)
	case r.Method == http.MethodPut:
		body, err := readBody(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		if f.fail != "" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>%s</Message></Error>`, f.fail)
			return
		}
		f.objects[strings.TrimPrefix(r.URL.Path, "/")] = body
		w.Header().Set("ETag", `"fake"`)
	default:
		http.Error(w, "not implemented", http.StatusNotImplemented)
	}
}

// readBody reads an upload. The client may send it in aws-chunked
// encoding: chunks of "<hex size>[;signature]\r\n<data>\r\n", then an
// empty chunk and optional trailers.
func readBody(r *http.Request) ([]byte, error) {
	if !strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		return io.ReadAll(r.Body)
	}
	br := bufio.NewReader(r.Body)
	var body []byte
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		sizeHex, _, _ := strings.Cut(strings.TrimSpace(line), ";")
		size, err := strconv.ParseInt(sizeHex, 16, 64)
		if err != nil {
			return nil, err
		}
		if size == 0 {
			return body, nil
		}
		chunk := make([]byte, size+2) // with the \r\n after it
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, err
		}
		body = append(body, chunk[:size]...)
	}
}

// s3Config is the S3 settings for a fake host.
func s3Config(url, prefix string) config.S3 {
	return config.S3{Endpoint: url, Bucket: "team", Prefix: prefix, AccessKeyID: testKeyID, SecretAccessKey: testSecretKey}
}

func TestBackupToS3(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	fake := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	for _, region := range []string{"", "eu-central-1"} {
		t.Run("region "+region, func(t *testing.T) {
			cfg := s3Config(srv.URL, "repo-speaks")
			cfg.Region = region
			s := f.service(t, config.Backup{S3: cfg})
			s.now = func() time.Time { return testNow.Add(time.Duration(len(region)) * time.Second) }
			b, err := s.Run(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if want := "S3 " + srv.URL + "/team/repo-speaks"; b.Targets != want {
				t.Errorf("targets = %q, want %q", b.Targets, want)
			}
			fake.mu.Lock()
			body, ok := fake.objects["team/repo-speaks/"+b.Name]
			auth := fake.auth[len(fake.auth)-1]
			fake.mu.Unlock()
			if !ok {
				t.Fatalf("no object team/repo-speaks/%s", b.Name)
			}
			if int64(len(body)) != b.Size {
				t.Errorf("object has %d bytes, recorded %d", len(body), b.Size)
			}
			if !strings.Contains(auth, "Credential="+testKeyID+"/") {
				t.Errorf("upload not signed with the access key: %q", auth)
			}
			checkBackupFile(t, body)
			f.checkNoTempFiles(t)
		})
	}
}

func TestBackupOneTargetFails(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	// The host echoes the secret key back in its message.
	fake := &fakeS3{objects: map[string][]byte{}, fail: "bad signature for " + testSecretKey}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	s := f.service(t, config.Backup{Dir: f.backupDir, S3: s3Config(srv.URL, "")})
	b, err := s.Run(ctx)
	if err == nil {
		t.Fatal("Run() with a failing S3 host: no error")
	}
	if b.Targets != "folder "+f.backupDir {
		t.Errorf("targets = %q, want the folder only", b.Targets)
	}
	if _, err := os.Stat(filepath.Join(f.backupDir, b.Name)); err != nil {
		t.Errorf("the folder did not get the backup: %v", err)
	}
	list, _ := f.store.Backups(ctx, 1)
	for _, msg := range []string{err.Error(), b.Error, list[0].Error} {
		if !strings.Contains(msg, "S3 "+srv.URL+"/team") || !strings.Contains(msg, "[hidden]") {
			t.Errorf("error = %q, want the S3 target named and the secret hidden", msg)
		}
		for _, secret := range []string{testSecretKey, testKeyID, testPassphrase} {
			if strings.Contains(msg, secret) {
				t.Errorf("error shows a secret: %q", msg)
			}
		}
	}
	f.checkNoTempFiles(t)
}

func TestTargetsHideSecrets(t *testing.T) {
	f := newFixture(t)
	s := f.service(t, config.Backup{Dir: "/mnt/backups", S3: s3Config("https://s3.example.com/", "rs")})
	got := strings.Join(s.Targets(), "; ")
	if want := "folder /mnt/backups; S3 https://s3.example.com/team/rs"; got != want {
		t.Errorf("Targets() = %q, want %q", got, want)
	}
}

// writeBackup makes a backup of the fixture and returns its path.
func (f *fixture) writeBackup(t *testing.T) string {
	t.Helper()
	b, err := f.service(t, config.Backup{Dir: f.backupDir}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(f.backupDir, b.Name)
}

func TestRestore(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	file := f.writeBackup(t)

	// While the server has the database open, nothing changes.
	if _, err := Restore(ctx, file, f.dbPath, []byte(testPassphrase), testNow); err == nil || !strings.Contains(err.Error(), "stop the server first") {
		t.Errorf("Restore() while open = %v, want stop the server first", err)
	}
	f.checkNoTempFiles(t)

	// The current database gets an account the backup lacks, so the two
	// can be told apart.
	if _, err := f.store.CreateUser(ctx, "later", "hash", testNow); err != nil {
		t.Fatal(err)
	}
	f.store.Close()

	r, err := Restore(ctx, file, f.dbPath, []byte(testPassphrase), testNow)
	if err != nil {
		t.Fatal(err)
	}
	wantAside := f.dbPath + ".before-restore-20261014T120000Z"
	if r.Aside != wantAside || r.Version != store.SchemaVersion() {
		t.Errorf("Restore() = %+v, want aside %s", r, wantAside)
	}
	checkDatabase(t, f.dbPath)
	checkUser(t, f.dbPath, "later", false)
	// The old database is kept whole, with the account made after the
	// backup.
	checkUser(t, wantAside, "later", true)
	f.checkNoTempFiles(t)
}

// checkUser checks whether the database at path has the account.
func checkUser(t *testing.T, path, username string, want bool) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = st.UserByUsername(ctx, username)
	if got := err == nil; got != want {
		t.Errorf("%s has %s = %v, want %v", filepath.Base(path), username, got, want)
	}
}

func TestRestoreIntoEmptyFolder(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	file := f.writeBackup(t)

	dbPath := filepath.Join(t.TempDir(), "repo-speaks.db")
	r, err := Restore(ctx, file, dbPath, []byte(testPassphrase), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if r.Aside != "" {
		t.Errorf("aside = %q, want none", r.Aside)
	}
	checkDatabase(t, dbPath)
}

func TestRestoreRefusals(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	file := f.writeBackup(t)
	f.store.Close()
	before, err := os.ReadFile(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// A backup from a newer version: make one by hand.
	newerPlain := filepath.Join(t.TempDir(), "newer.db")
	db, err := sql.Open("sqlite", "file:"+newerPlain)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(fmt.Sprintf("CREATE TABLE users (id INTEGER); PRAGMA user_version = %d", store.SchemaVersion()+1))
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	newerFile := filepath.Join(t.TempDir(), "newer.db.gpg")
	encryptTo(t, newerPlain, newerFile)

	tests := []struct {
		name       string
		file       string
		passphrase string
		want       string
	}{
		{"wrong passphrase", file, "another long passphrase", "wrong passphrase"},
		{"missing file", filepath.Join(t.TempDir(), "missing.gpg"), testPassphrase, "no such file"},
		{"newer version", newerFile, testPassphrase, "newer version of Repo Speaks"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Restore(ctx, tt.file, f.dbPath, []byte(tt.passphrase), testNow)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Restore() = %v, want %q", err, tt.want)
			}
			after, err := os.ReadFile(f.dbPath)
			if err != nil || !bytes.Equal(after, before) {
				t.Errorf("database changed after a refused restore (%v)", err)
			}
			f.checkNoTempFiles(t)
		})
	}
}

// encryptTo encrypts the file at src into dst with the test passphrase.
func encryptTo(t *testing.T, src, dst string) {
	t.Helper()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err := encryptFile(context.Background(), out, src, "x.db", []byte(testPassphrase), testNow); err != nil {
		t.Fatal(err)
	}
}

// TestGPGDecrypts checks that plain gpg opens a backup, as the README
// says. It needs gpg on the machine.
func TestGPGDecrypts(t *testing.T) {
	gpg, err := exec.LookPath("gpg")
	if err != nil {
		t.Skip("gpg is not installed")
	}
	f := newFixture(t)
	file := f.writeBackup(t)

	// A home of its own keeps the test away from the user's keys. The
	// agent's socket must fit a short path, so it is not under t.TempDir.
	home, err := os.MkdirTemp("", "gpg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		kill := exec.Command("gpgconf", "--kill", "gpg-agent")
		kill.Env = append(os.Environ(), "GNUPGHOME="+home)
		kill.Run()
		os.RemoveAll(home)
	})
	out := filepath.Join(t.TempDir(), "plain.db")
	cmd := exec.Command(gpg, "--batch", "--quiet", "--pinentry-mode", "loopback", "--no-symkey-cache",
		"--passphrase", testPassphrase, "--output", out, "--decrypt", file)
	cmd.Env = append(os.Environ(), "GNUPGHOME="+home)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gpg: %v\n%s", err, msg)
	}
	checkDatabase(t, out)
}
