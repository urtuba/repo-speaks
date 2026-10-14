package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urtuba/repo-speaks/internal/config"
)

// Secret settings for the backup tests. No page or log may show them.
const (
	backupPassphrase = "correct horse battery staple"
	backupKeyID      = "AKIDEXAMPLE1234"
	backupSecretKey  = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

func TestBackupsPageOwnerOnly(t *testing.T) {
	app := newTestApp(t)
	app.createUser("samed", true)
	app.createUser("ayse", false)
	app.useBackups(config.Backup{Dir: t.TempDir(), Passphrase: backupPassphrase})

	owner := app.login("samed")
	if rec := app.get("/backups", owner); rec.Code != http.StatusOK {
		t.Errorf("owner: status = %d, want 200", rec.Code)
	}
	if rec := app.get("/", owner); !strings.Contains(rec.Body.String(), `<a href="/backups">Backups</a>`) {
		t.Error("the owner's top bar has no Backups link")
	}

	member := app.login("ayse")
	if rec := app.get("/", member); strings.Contains(rec.Body.String(), `href="/backups"`) {
		t.Error("a member's top bar links to Backups")
	}
	for _, rec := range []*httptest.ResponseRecorder{
		app.get("/backups", member),
		app.get("/backups/list", member),
		app.post("/backups", nil, member),
	} {
		if rec.Code != http.StatusForbidden {
			t.Errorf("member: status = %d, want 403", rec.Code)
		}
	}
	if status, _ := app.store.OpenBackupJob(context.Background()); status != "" {
		t.Errorf("a member queued a backup (%s)", status)
	}

	if rec := app.get("/backups", nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("logged out: status = %d, Location = %q, want the login page", rec.Code, rec.Header().Get("Location"))
	}
}

func TestBackupsPageOff(t *testing.T) {
	app := newTestApp(t)
	app.createUser("samed", true)
	owner := app.login("samed")

	body := app.get("/backups", owner).Body.String()
	if !strings.Contains(body, "Backups are off.") || strings.Contains(body, "Back up now") {
		t.Errorf("page does not say backups are off, or has the button:\n%s", body)
	}
	if rec := app.post("/backups", nil, owner); rec.Code != http.StatusBadRequest {
		t.Errorf("POST with backups off: status = %d, want 400", rec.Code)
	}
}

func TestBackUpNow(t *testing.T) {
	ctx := context.Background()
	app := newTestApp(t)
	app.createUser("samed", true)
	dir := t.TempDir()
	app.useBackups(config.Backup{Dir: dir, Passphrase: backupPassphrase})
	owner := app.login("samed")

	body := app.get("/backups", owner).Body.String()
	for _, want := range []string{"Backups are on.", "folder " + dir, "Back up now", "No backups yet."} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
	if strings.Contains(body, `hx-get="/backups/list"`) {
		t.Error("the list polls with no backup open")
	}

	// Pressing twice queues one backup.
	for _, notice := range []string{"queued", "open"} {
		rec := app.post("/backups", nil, owner)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/backups?notice="+notice {
			t.Errorf("POST: status = %d, Location = %q, want notice %s", rec.Code, rec.Header().Get("Location"), notice)
		}
	}
	body = app.get("/backups?notice=queued", owner).Body.String()
	for _, want := range []string{"Backup queued.", `<span class="badge">queued</span>`, `hx-get="/backups/list" hx-trigger="every 5s"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
	if body := app.get("/backups?notice=<b>hi</b>", owner).Body.String(); strings.Contains(body, "hi</b>") {
		t.Error("the URL put text on the page")
	}

	// The jobs loop claims the job and makes the backup.
	job, err := app.store.ClaimJob(ctx, app.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	frag := strings.TrimSpace(app.get("/backups/list", owner).Body.String())
	if !strings.HasPrefix(frag, `<div id="backup-list" hx-get=`) || !strings.Contains(frag, "running") || strings.Contains(frag, "<html") {
		t.Errorf("fragment = %q, want the running list alone", frag)
	}
	b, err := app.backups.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.FinishJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	frag = app.get("/backups/list", owner).Body.String()
	if strings.Contains(frag, `hx-get="/backups/list"`) {
		t.Error("the list still polls after the backup")
	}
	for _, want := range []string{b.Name, "folder " + dir, `<span class="badge">done</span>`, " kB"} {
		if !strings.Contains(frag, want) {
			t.Errorf("list does not contain %q:\n%s", want, frag)
		}
	}
}

func TestBackupsPageHidesSecrets(t *testing.T) {
	ctx := context.Background()
	app := newTestApp(t)
	app.createUser("samed", true)
	// A host that refuses every request and echoes the secret key back.
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>no access for %s</Message></Error>`, backupSecretKey)
	}))
	defer host.Close()
	app.useBackups(config.Backup{
		S3: config.S3{
			Endpoint: host.URL, Bucket: "team", Region: "us-east-1", Prefix: "repo-speaks",
			AccessKeyID: backupKeyID, SecretAccessKey: backupSecretKey,
		},
		Passphrase: backupPassphrase,
	})
	if _, err := app.backups.Run(ctx); err == nil {
		t.Fatal("Run() against a refusing host: no error")
	}

	body := app.get("/backups", app.login("samed")).Body.String()
	for _, want := range []string{"S3 " + host.URL + "/team/repo-speaks", "[hidden]"} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
	for _, secret := range []string{backupPassphrase, backupKeyID, backupSecretKey} {
		if strings.Contains(body, secret) {
			t.Errorf("page shows %q", secret)
		}
		if strings.Contains(app.logs.String(), secret) {
			t.Errorf("log shows %q", secret)
		}
	}
}

func TestSizeText(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 999: "999 B", 1000: "1.0 kB", 4_200_000: "4.2 MB", 3_100_000_000: "3.1 GB"} {
		if got := sizeText(n); got != want {
			t.Errorf("sizeText(%d) = %q, want %q", n, got, want)
		}
	}
}
