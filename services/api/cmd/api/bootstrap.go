package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/pkg/config"
)

// `api bootstrap` (ADR-028, contracts §11/§27): idempotently creates the owner user, org, project and a `bootstrap`
// ingest key, then prints the project id and DSN. Used by `make self` and CI instead of seed SQL.
//
// Key handling (a key's plaintext cannot be recovered from the database):
//   - --ingest-key-out file exists and holds the active `bootstrap` key → reused, DSN rebuilt from it;
//   - file missing / stale (key revoked or another database) → active `bootstrap` keys are revoked, a new key is
//     minted and written to the file (parent dir 0700, file 0600);
//   - no --ingest-key-out: a new key is minted only when none exists (printed once); an existing one is kept
//     and the DSN is empty.

type bootstrapOpts struct {
	Org, Project, Owner, KeyOut, IngestURL string
	JSON                                   bool
}

type bootstrapJSON struct {
	OrgID     int64  `json:"orgId"`
	ProjectID int64  `json:"projectId"`
	DSN       string `json:"dsn"`
	Created   struct {
		Org     bool `json:"org"`
		Project bool `json:"project"`
		Key     bool `json:"key"`
	} `json:"created"`
}

// bootstrapMain parses args (after "bootstrap") and returns the exit code. stdout gets only the result.
func bootstrapMain(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("api bootstrap", flag.ContinueOnError)
	fl.SetOutput(stderr)
	var o bootstrapOpts
	fl.StringVar(&o.Org, "org", "", "org name (found among the orgs the owner owns, else created)")
	fl.StringVar(&o.Project, "project", "", "project name (found by slug or name in the org, else created)")
	fl.StringVar(&o.Owner, "owner", "", "owner email (user created when unknown)")
	fl.StringVar(&o.KeyOut, "ingest-key-out", "", "file holding the plaintext `bootstrap` ingest key (read when present, written when a key is minted)")
	fl.StringVar(&o.IngestURL, "ingest-url", config.String("TT_PUBLIC_INGEST_URL", store.DefaultPublicIngestURL), "public ingest base URL for the DSN")
	fl.BoolVar(&o.JSON, "json", false, `print {"orgId","projectId","dsn","created":{"org","project","key"}} instead of key=value lines`)
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() > 0 || o.Org == "" || o.Project == "" || o.Owner == "" {
		fmt.Fprintln(stderr, "usage: api bootstrap --org <name> --project <name> --owner <email> [--ingest-key-out <file>] [--json]")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, config.Load().PostgresURL)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		fmt.Fprintln(stderr, "bootstrap: postgres:", err)
		return 1
	}
	defer pool.Close()
	if err := runBootstrap(ctx, store.New(nil, pool), o, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, "bootstrap:", err)
		return 1
	}
	return 0
}

func runBootstrap(ctx context.Context, db *store.DB, o bootstrapOpts, stdout, stderr io.Writer) error {
	in := store.BootstrapInput{Org: o.Org, Project: o.Project, OwnerEmail: o.Owner, CanStore: o.KeyOut != ""}
	if o.KeyOut != "" {
		b, err := os.ReadFile(o.KeyOut)
		switch {
		case err == nil:
			in.KnownKey = store.KeyFromDSN(string(b))
		case !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("read %s: %w", o.KeyOut, err)
		}
	}
	res, err := db.Bootstrap(ctx, in)
	if err != nil {
		return err
	}
	switch {
	case res.KeyMinted && len(res.RevokedHashes) > 0:
		fmt.Fprintf(stderr, "bootstrap: rotated ingest key %q of project %d (revoked %d; auth caches drop them within 60 s)\n",
			store.BootstrapKeyName, res.ProjectID, len(res.RevokedHashes))
	case res.KeyMinted:
		fmt.Fprintf(stderr, "bootstrap: created ingest key %q (%s) for project %d\n", store.BootstrapKeyName, res.KeyPrefix, res.ProjectID)
	case res.Key == "":
		fmt.Fprintf(stderr, "bootstrap: kept existing ingest key %s; its plaintext is not recoverable (pass --ingest-key-out to rotate it)\n", res.KeyPrefix)
	default:
		fmt.Fprintf(stderr, "bootstrap: reusing ingest key %s from %s\n", res.KeyPrefix, o.KeyOut)
	}
	if res.KeyMinted && o.KeyOut != "" {
		if err := writeSecret(o.KeyOut, res.Key+"\n"); err != nil {
			return fmt.Errorf("ingest key %s was created but could not be saved (the next run rotates it): %w", res.KeyPrefix, err)
		}
	}
	dsn := ""
	if res.Key != "" {
		if dsn, err = store.DSN(o.IngestURL, res.Key, res.ProjectID); err != nil {
			return err
		}
	}
	if o.JSON {
		var out bootstrapJSON
		out.OrgID, out.ProjectID, out.DSN = res.OrgID, res.ProjectID, dsn
		out.Created.Org, out.Created.Project, out.Created.Key = res.OrgCreated, res.ProjectCreated, res.KeyMinted
		return json.NewEncoder(stdout).Encode(out)
	}
	_, err = fmt.Fprintf(stdout, "org_id=%d\nproject_id=%d\ndsn=%s\n", res.OrgID, res.ProjectID, dsn)
	return err
}

// writeSecret writes data to path (0600) atomically, creating the parent directory (0700).
func writeSecret(path, data string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) //nolint:errcheck // gone after the rename
	if err := f.Chmod(0o600); err == nil {
		_, err = io.Copy(f, strings.NewReader(data))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
