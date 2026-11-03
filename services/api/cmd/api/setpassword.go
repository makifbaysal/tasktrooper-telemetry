package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tasktrooper/telemetry/services/api/internal/auth"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/pkg/config"
)

// `api set-password` (ADR-035, contracts §15): gives a user an email + password login (POST /auth/password-login,
// TT_AUTH_PASSWORD=true), e.g. the shared viewer account of the public demo. Idempotent:
//   - the user is created when the email is unknown (--name, else the email's local part);
//   - the password is read from stdin only (never argv); the same password again changes nothing, a new one replaces
//     the hash and signs out the user's sessions;
//   - --org (id or exact name) or --project (id: its org) adds the membership when missing, with --role (default
//     viewer); an existing membership keeps its role unless --role is passed (the last owner is never demoted).

type setPasswordOpts struct {
	Email, Name, Org, Role string
	Project                int64
	Stdin                  bool
}

type passwordStore interface {
	PasswordHash(ctx context.Context, email string) (int64, string, error)
	SetPassword(ctx context.Context, in store.SetPasswordInput) (*store.SetPasswordResult, error)
}

const setPasswordUsage = "usage: api set-password --email <email> [--name <name>] [--org <org id or name> | --project <project id>] [--role viewer|member|admin] --password-stdin"

func setPasswordMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("api set-password", flag.ContinueOnError)
	fl.SetOutput(stderr)
	var o setPasswordOpts
	fl.StringVar(&o.Email, "email", "", "user email (created when unknown)")
	fl.StringVar(&o.Name, "name", "", "display name (set when given)")
	fl.StringVar(&o.Org, "org", "", "org id or exact org name to ensure a membership in")
	fl.Int64Var(&o.Project, "project", 0, "project id: ensure a membership in its org (instead of --org)")
	fl.StringVar(&o.Role, "role", "", "membership role viewer|member|admin (default: viewer for a new membership, an existing one is kept)")
	fl.BoolVar(&o.Stdin, "password-stdin", false, "read the password from stdin (required; trailing newline is stripped)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() > 0 || o.Email == "" || !o.Stdin || (o.Org != "" && o.Project != 0) ||
		(o.Role != "" && o.Role != "viewer" && o.Role != "member" && o.Role != "admin") {
		fmt.Fprintln(stderr, setPasswordUsage)
		return 2
	}
	pw, err := readPassword(stdin)
	if err == nil {
		err = auth.ValidatePassword(pw)
	}
	if err != nil {
		fmt.Fprintln(stderr, "set-password:", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, config.Load().PostgresURL)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		fmt.Fprintln(stderr, "set-password: postgres:", err)
		return 1
	}
	defer pool.Close()
	if err := runSetPassword(ctx, store.New(nil, pool), o, pw, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, "set-password:", err)
		return 1
	}
	return 0
}

// readPassword reads the first line of r (up to 4 KiB) without its line ending.
func readPassword(r io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(r, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", errors.New("no password on stdin")
	}
	return line, nil
}

func runSetPassword(ctx context.Context, db passwordStore, o setPasswordOpts, pw string, stdout, stderr io.Writer) error {
	if err := auth.ValidatePassword(pw); err != nil {
		return err
	}
	in := store.SetPasswordInput{Email: o.Email, Name: o.Name, Org: o.Org, ProjectID: o.Project, Role: o.Role}
	_, cur, err := db.PasswordHash(ctx, o.Email)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if cur == "" || auth.CheckPassword(cur, pw) != nil {
		if in.Hash, err = auth.HashPassword(pw); err != nil {
			return err
		}
	}
	res, err := db.SetPassword(ctx, in)
	if err != nil {
		return err
	}
	switch {
	case res.UserCreated:
		fmt.Fprintf(stderr, "set-password: created user %d\n", res.UserID)
	case res.PasswordChanged:
		fmt.Fprintf(stderr, "set-password: password of user %d replaced (%d sessions signed out)\n", res.UserID, res.SessionsRevoked)
	default:
		fmt.Fprintf(stderr, "set-password: user %d unchanged\n", res.UserID)
	}
	if res.MemberAdded {
		fmt.Fprintf(stderr, "set-password: added to org %d as %s\n", res.OrgID, res.Role)
	} else if res.RoleChanged {
		fmt.Fprintf(stderr, "set-password: role in org %d changed to %s\n", res.OrgID, res.Role)
	}
	pwState := "unchanged"
	if res.PasswordChanged {
		pwState = "changed"
	}
	_, err = fmt.Fprintf(stdout, "user_id=%d\norg_id=%d\nrole=%s\npassword=%s\n", res.UserID, res.OrgID, res.Role, pwState)
	return err
}
