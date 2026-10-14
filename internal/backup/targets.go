package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/urtuba/repo-speaks/internal/config"
)

// folder is a mounted folder, such as an external disk or a NAS share.
type folder struct {
	dir string
}

func (f folder) String() string { return "folder " + f.dir }

// put writes the file under a temporary name in the folder, then
// renames it, so the folder never holds half a backup under the real
// name. The folder is never created: a missing folder is often a disk
// that is not mounted, and the backup must not land on the local disk
// instead.
func (f folder) put(ctx context.Context, name string, r *io.SectionReader) (err error) {
	info, err := os.Stat(f.dir)
	if err != nil {
		return fmt.Errorf("the folder is missing; is the disk mounted? (%w)", err)
	}
	if !info.IsDir() {
		return errors.New("not a folder")
	}
	final := filepath.Join(f.dir, name)
	if _, err := os.Lstat(final); err == nil {
		return fmt.Errorf("%s already exists", name)
	}

	// CreateTemp makes the file with mode 0600.
	tmp, err := os.CreateTemp(f.dir, "."+name+".*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := io.Copy(tmp, ctxReader{ctx, r}); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return err
	}
	syncDir(f.dir)
	return nil
}

// syncDir writes a folder's entries to disk, so a rename survives a
// power cut. Some file systems cannot sync a folder; the file itself is
// already on disk then, so a failure here is ignored.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	d.Sync()
	d.Close()
}

// bucket is an S3-compatible bucket, such as on AWS, Backblaze B2,
// Cloudflare R2, Hetzner or a MinIO server.
type bucket struct {
	client   *minio.Client
	endpoint string
	name     string
	prefix   string
}

// newBucket makes a client for the bucket. It does not connect yet.
// The client picks the bucket's address style itself: path style, as
// MinIO needs, for every host but AWS and a few that prefer the other.
func newBucket(cfg config.S3) (bucket, error) {
	host, secure, err := cfg.EndpointHost()
	if err != nil {
		return bucket{}, err
	}
	client, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(string(cfg.AccessKeyID), string(cfg.SecretAccessKey), ""),
		Secure: secure,
		Region: cfg.Region,
	})
	if err != nil {
		return bucket{}, err
	}
	return bucket{client: client, endpoint: strings.TrimRight(cfg.Endpoint, "/"), name: cfg.Bucket, prefix: cfg.Prefix}, nil
}

func (b bucket) String() string {
	return "S3 " + b.endpoint + "/" + path.Join(b.name, b.prefix)
}

// put uploads the file. A large file goes up in parts, read straight
// from the file on disk.
func (b bucket) put(ctx context.Context, name string, r *io.SectionReader) error {
	key := path.Join(b.prefix, name)
	_, err := b.client.PutObject(ctx, b.name, key, r, r.Size(), minio.PutObjectOptions{
		ContentType: "application/octet-stream",
	})
	return err
}
