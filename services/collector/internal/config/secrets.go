package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// ResolveSecret resolves a credentials_ref: "env:NAME" reads $NAME, "file:/path" reads the file
// (trailing newline trimmed). An empty ref resolves to "". Any other scheme is an error: the
// collector never talks to a secret store directly, the deployment mounts secrets as env/files.
func ResolveSecret(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", nil
	}
	scheme, rest, ok := strings.Cut(ref, ":")
	if !ok {
		return "", fmt.Errorf("credentials ref %q: want env:NAME or file:/path", ref)
	}
	switch scheme {
	case "env":
		v, ok := os.LookupEnv(rest)
		if !ok {
			return "", fmt.Errorf("credentials ref %q: env var %s is not set", ref, rest)
		}
		return v, nil
	case "file":
		b, err := os.ReadFile(rest)
		if err != nil {
			return "", fmt.Errorf("credentials ref %q: %w", ref, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return "", fmt.Errorf("credentials ref %q: unsupported scheme %q (want env: or file:)", ref, scheme)
}

// LooksLikeDSN reports whether a resolved secret is a full connection string rather than a
// bare password.
func LooksLikeDSN(s string) bool {
	return strings.Contains(s, "://") || strings.Contains(s, "@tcp(") || strings.Contains(s, "@unix(") ||
		(strings.Contains(s, "=") && strings.Contains(s, " ") && strings.Contains(s, "host="))
}

// ConnString builds the driver connection string for a database target:
//   - inline dsn wins,
//   - else a credentials secret that looks like a DSN is used as is,
//   - else the secret is a password and the DSN is built from host/port/user/database.
//
// Shapes: postgres → postgres://user:pass@host:port/db?sslmode=…; mysql → user:pass@tcp(host:port)/db;
// redis → redis://[user:pass@]host:port/0; mongodb → mongodb://user:pass@host:port.
func (d Database) ConnString() (string, error) {
	if d.DSN != "" {
		return os.ExpandEnv(d.DSN), nil
	}
	secret, err := ResolveSecret(d.CredentialsRef)
	if err != nil {
		return "", err
	}
	if LooksLikeDSN(secret) {
		return secret, nil
	}
	if d.Host == "" {
		return "", errors.New("no dsn, credentials DSN or host configured")
	}
	hostport := net.JoinHostPort(d.Host, strconv.Itoa(d.Port))
	user := d.User
	switch d.Engine {
	case "postgres":
		if user == "" {
			user = DefaultUserSQL
		}
		u := url.URL{Scheme: "postgres", Host: hostport, Path: "/" + firstNonEmpty(d.Database, "postgres")}
		if secret != "" {
			u.User = url.UserPassword(user, secret)
		} else {
			u.User = url.User(user)
		}
		q := url.Values{}
		q.Set("sslmode", firstNonEmpty(d.TLS, "prefer"))
		q.Set("application_name", "trooper-agent")
		u.RawQuery = q.Encode()
		return u.String(), nil
	case "mysql":
		if user == "" {
			user = DefaultUserSQL
		}
		cred := user
		if secret != "" {
			cred += ":" + secret
		}
		dsn := fmt.Sprintf("%s@tcp(%s)/%s", cred, hostport, d.Database)
		if d.TLS != "" {
			dsn += "?tls=" + url.QueryEscape(d.TLS)
		}
		return dsn, nil
	case "redis":
		scheme := "redis"
		if d.TLS == "true" {
			scheme = "rediss"
		}
		u := url.URL{Scheme: scheme, Host: hostport, Path: "/0"}
		if secret != "" {
			u.User = url.UserPassword(user, secret)
		}
		return u.String(), nil
	case "mongodb":
		if user == "" {
			user = DefaultUserSQL
		}
		u := url.URL{Scheme: "mongodb", Host: hostport}
		if secret != "" {
			u.User = url.UserPassword(user, secret)
		}
		return u.String(), nil
	}
	return "", fmt.Errorf("unknown engine %q", d.Engine)
}

// Credentials returns (user, password) for a broker. The secret may be "user:password" or a
// bare password (user from the `user` field, default "trooper").
func (b Broker) Credentials() (user, pass string, err error) {
	secret, err := ResolveSecret(b.CredentialsRef)
	if err != nil {
		return "", "", err
	}
	user = firstNonEmpty(b.User, DefaultUserMQ)
	if u, p, ok := strings.Cut(secret, ":"); ok && u != "" && !strings.Contains(u, "/") {
		return u, p, nil
	}
	return user, secret, nil
}
