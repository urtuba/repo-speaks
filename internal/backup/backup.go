// Package backup makes encrypted copies of the database and sends them
// to the backup targets: a mounted folder, S3-compatible storage, or
// both. It also puts a backup back in place of the database.
//
// A backup holds the database only. The repository clones are left out,
// since a sync rebuilds them. The app never deletes old backups; the
// target's own rules do that.
package backup

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/ProtonMail/go-crypto/openpgp/s2k"

	"github.com/urtuba/repo-speaks/internal/config"
	"github.com/urtuba/repo-speaks/internal/store"
)

// ErrOff is returned when no backup target is set.
var ErrOff = errors.New("backups are off: set REPO_SPEAKS_BACKUP_DIR or the REPO_SPEAKS_BACKUP_S3_ settings, and REPO_SPEAKS_BACKUP_PASSPHRASE")

// ErrPassphrase is returned when a backup does not open with the
// passphrase.
var ErrPassphrase = errors.New("wrong passphrase, or the file is not a Repo Speaks backup")

// stamp is the format of times in file names: UTC, without separators
// that some file systems refuse.
const stamp = "20060102T150405Z"

// recordTimeout limits the database write that records a backup, which
// still runs during a shutdown.
const recordTimeout = 5 * time.Second

// pgpConfig makes files that any gpg can open: AES-256 in the classic
// integrity-protected format, with zlib compression. The passphrase is
// stretched with the slowest iterated and salted S2K setting the format
// allows. Argon2 and the newer AEAD format are stronger, but older gpg
// versions, such as the one in Debian 12, cannot read them.
var pgpConfig = &packet.Config{
	DefaultCipher:          packet.CipherAES256,
	DefaultCompressionAlgo: packet.CompressionZLIB,
	S2KConfig: &s2k.Config{
		S2KMode:  s2k.IteratedSaltedS2K,
		Hash:     crypto.SHA256,
		S2KCount: 65011712,
	},
}

// target is a place a backup goes.
type target interface {
	// String says where the target is, without secrets, for pages and
	// logs.
	String() string
	// put stores the encrypted file r under name.
	put(ctx context.Context, name string, r *io.SectionReader) error
}

// Service makes backups. With no target set it is off, and Run returns
// ErrOff.
type Service struct {
	store      *store.Store
	dataDir    string
	targets    []target
	passphrase []byte
	secrets    []string // values that must never reach an error
	now        func() time.Time
	mu         sync.Mutex // one backup at a time in this process
}

// New returns the service for the settings in cfg, which Check has
// accepted. Temporary files go to dataDir, next to the database.
func New(st *store.Store, dataDir string, cfg config.Backup) (*Service, error) {
	s := &Service{store: st, dataDir: dataDir, passphrase: []byte(cfg.Passphrase), now: time.Now}
	for _, secret := range []config.Secret{cfg.Passphrase, cfg.S3.AccessKeyID, cfg.S3.SecretAccessKey} {
		if secret != "" {
			s.secrets = append(s.secrets, string(secret))
		}
	}
	if cfg.Dir != "" {
		s.targets = append(s.targets, folder{dir: cfg.Dir})
	}
	if cfg.S3.Set() {
		b, err := newBucket(cfg.S3)
		if err != nil {
			return nil, fmt.Errorf("backup: %s", s.redact(err.Error()))
		}
		s.targets = append(s.targets, b)
	}
	return s, nil
}

// On reports whether backups are on.
func (s *Service) On() bool {
	return len(s.targets) > 0
}

// Targets says where backups go, without secrets.
func (s *Service) Targets() []string {
	var list []string
	for _, t := range s.targets {
		list = append(list, t.String())
	}
	return list
}

// FileName is the name of a backup made at t from a database at the
// schema version, such as repo-speaks-20261014T120000Z-schema0008.db.gpg.
func FileName(t time.Time, version int) string {
	return fmt.Sprintf("repo-speaks-%s-schema%04d.db.gpg", t.UTC().Format(stamp), version)
}

// Run makes one backup and sends it to every target. It records the
// backup in the database, also when it failed. A failed target does not
// stop the others; the error names each failed one. The returned
// backup says which targets got the file.
func (s *Service) Run(ctx context.Context) (store.Backup, error) {
	if !s.On() {
		return store.Backup{}, ErrOff
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	b := store.Backup{StartedAt: s.now()}
	b.Name = FileName(b.StartedAt, store.SchemaVersion())
	size, done, err := s.write(ctx, b.Name)
	if len(done) > 0 {
		b.Size = size
		b.Targets = strings.Join(done, ", ")
	}
	b.FinishedAt = s.now()
	if err != nil {
		err = errors.New(s.redact(err.Error()))
		b.Error = err.Error()
	}

	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if rerr := s.store.AddBackup(wctx, b); rerr != nil {
		if err == nil {
			return b, rerr
		}
		return b, fmt.Errorf("%w; %w", err, rerr)
	}
	return b, err
}

// write copies the database, encrypts the copy and sends it to the
// targets. It returns the encrypted size and the targets that got it.
// Both temporary files are removed, also on failure. The plain copy
// holds every key and token, so it goes before the uploads start.
func (s *Service) write(ctx context.Context, name string) (int64, []string, error) {
	plain, err := tempFile(s.dataDir, "repo-speaks-backup-*.db.tmp")
	if err != nil {
		return 0, nil, err
	}
	defer os.Remove(plain)
	if err := s.store.CopyTo(ctx, plain); err != nil {
		return 0, nil, err
	}

	enc, err := os.CreateTemp(s.dataDir, "repo-speaks-backup-*.db.gpg.tmp")
	if err != nil {
		return 0, nil, fmt.Errorf("backup: %w", err)
	}
	defer func() {
		enc.Close()
		os.Remove(enc.Name())
	}()
	if err := encryptFile(ctx, enc, plain, name, s.passphrase, s.now()); err != nil {
		return 0, nil, err
	}
	if err := os.Remove(plain); err != nil {
		return 0, nil, fmt.Errorf("backup: %w", err)
	}
	info, err := enc.Stat()
	if err != nil {
		return 0, nil, fmt.Errorf("backup: %w", err)
	}

	var done, failed []string
	for _, t := range s.targets {
		if err := t.put(ctx, name, io.NewSectionReader(enc, 0, info.Size())); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", t, err))
			continue
		}
		done = append(done, t.String())
	}
	if len(failed) > 0 {
		return info.Size(), done, errors.New("backup failed for " + strings.Join(failed, "; "))
	}
	return info.Size(), done, nil
}

// tempFile makes an empty file with mode 0600 in dir and returns its
// path.
func tempFile(dir, pattern string) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("backup: %w", err)
	}
	return f.Name(), nil
}

// encryptFile encrypts the file at src into dst with the passphrase. It
// streams, so the database is never held in memory. name is the
// backup's file name; gpg can restore the inner name from it.
func encryptFile(ctx context.Context, dst *os.File, src, name string, passphrase []byte, now time.Time) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	defer in.Close()
	hints := &openpgp.FileHints{IsBinary: true, FileName: strings.TrimSuffix(name, ".gpg"), ModTime: now}
	w, err := openpgp.SymmetricallyEncrypt(dst, passphrase, hints, pgpConfig)
	if err != nil {
		return fmt.Errorf("encrypt backup: %w", err)
	}
	if _, err := io.Copy(w, ctxReader{ctx, in}); err != nil {
		return fmt.Errorf("encrypt backup: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("encrypt backup: %w", err)
	}
	if err := dst.Sync(); err != nil {
		return fmt.Errorf("encrypt backup: %w", err)
	}
	return nil
}

// decrypt decrypts a backup from src into dst with the passphrase. It
// returns ErrPassphrase when the passphrase does not open it. A file
// that was changed after it was made fails the integrity check at the
// end.
func decrypt(ctx context.Context, dst io.Writer, src io.Reader, passphrase []byte) error {
	tried := false
	prompt := func([]openpgp.Key, bool) ([]byte, error) {
		// The library asks again after a wrong passphrase; there is no
		// second one to give.
		if tried {
			return nil, ErrPassphrase
		}
		tried = true
		return passphrase, nil
	}
	md, err := openpgp.ReadMessage(src, nil, prompt, pgpConfig)
	if errors.Is(err, ErrPassphrase) || errors.Is(err, pgperrors.ErrKeyIncorrect) {
		return ErrPassphrase
	}
	if err != nil {
		return fmt.Errorf("the file is not an encrypted backup: %w", err)
	}
	if !md.IsSymmetricallyEncrypted {
		return errors.New("the file is not encrypted with a passphrase")
	}
	if _, err := io.Copy(dst, ctxReader{ctx, md.UnverifiedBody}); err != nil {
		return fmt.Errorf("decrypt backup: %w", err)
	}
	return nil
}

// redact replaces every secret in msg with a mark. Errors from the
// storage host or the network may echo a setting back.
func (s *Service) redact(msg string) string {
	for _, secret := range s.secrets {
		msg = strings.ReplaceAll(msg, secret, "[hidden]")
	}
	return msg
}

// ctxReader stops a long copy when ctx ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
