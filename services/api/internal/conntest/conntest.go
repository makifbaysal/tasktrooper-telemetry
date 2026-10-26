// Package conntest checks that a database or broker is reachable with the given credentials, the way the
// collector will connect: TCP dial for every engine, plus Postgres `SELECT version()`, Redis PING,
// a Kafka metadata request (franz-go) and RabbitMQ `GET /api/overview`.
package conntest

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/plain"
)

// Timeout bounds every test.
var Timeout = 5 * time.Second

// Run executes the test described by in. It never returns an error: failures are reported in the result.
func Run(ctx context.Context, in model.ConnTestInput) model.ConnTestResult {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	start := time.Now()
	var res model.ConnTestResult
	var err error
	switch in.Kind {
	case "postgres":
		res, err = postgres(ctx, in)
	case "mysql", "mongodb":
		res, err = dialOnly(ctx, in)
	case "redis":
		res, err = redis(ctx, in)
	case "kafka":
		res, err = kafka(ctx, in)
	case "rabbitmq":
		res, err = rabbit(ctx, in)
	default:
		err = fmt.Errorf("unknown kind %q (want postgres|mysql|redis|mongodb|kafka|rabbitmq)", in.Kind)
	}
	res.LatencyMs = float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		res.OK, res.Message = false, err.Error()
	} else {
		res.OK = true
	}
	return res
}

func addr(in model.ConnTestInput, def int) (string, error) {
	if in.Host == "" {
		return "", fmt.Errorf("host is required")
	}
	host, port := in.Host, in.Port
	if h, p, err := net.SplitHostPort(in.Host); err == nil {
		host = h
		port, _ = strconv.Atoi(p)
	}
	if port == 0 {
		port = def
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

var defaults = map[string]int{"postgres": 5432, "mysql": 3306, "redis": 6379, "mongodb": 27017}

func dial(ctx context.Context, a string) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", a)
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s: %w", a, err)
	}
	return c, nil
}

func dialOnly(ctx context.Context, in model.ConnTestInput) (model.ConnTestResult, error) {
	a, err := addr(in, defaults[in.Kind])
	if err != nil {
		return model.ConnTestResult{}, err
	}
	c, err := dial(ctx, a)
	if err != nil {
		return model.ConnTestResult{}, err
	}
	c.Close()
	return model.ConnTestResult{Message: "TCP connection to " + a + " OK (protocol login is checked by the collector)"}, nil
}

func postgres(ctx context.Context, in model.ConnTestInput) (model.ConnTestResult, error) {
	a, err := addr(in, 5432)
	if err != nil {
		return model.ConnTestResult{}, err
	}
	c, err := dial(ctx, a)
	if err != nil {
		return model.ConnTestResult{}, err
	}
	c.Close()
	if in.User == "" {
		return model.ConnTestResult{Message: "TCP connection to " + a + " OK; give a user to check login"}, nil
	}
	db := in.Database
	if db == "" {
		db = "postgres"
	}
	ssl := "prefer"
	if in.TLS {
		ssl = "require"
	}
	u := url.URL{Scheme: "postgres", User: url.UserPassword(in.User, in.Password), Host: a, Path: "/" + db, RawQuery: "sslmode=" + ssl + "&connect_timeout=5"}
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		return model.ConnTestResult{}, fmt.Errorf("login failed: %w", err)
	}
	defer conn.Close(context.Background())
	var one int
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		return model.ConnTestResult{}, err
	}
	var version string
	_ = conn.QueryRow(ctx, "SHOW server_version").Scan(&version)
	var statements bool
	_ = conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_stat_statements')").Scan(&statements)
	msg := "Connected · PostgreSQL " + version
	if statements {
		msg += " · pg_stat_statements enabled"
	} else {
		msg += " · pg_stat_statements not installed (no top queries)"
	}
	return model.ConnTestResult{Message: msg, Version: version}, nil
}

func redis(ctx context.Context, in model.ConnTestInput) (model.ConnTestResult, error) {
	a, err := addr(in, 6379)
	if err != nil {
		return model.ConnTestResult{}, err
	}
	c, err := dial(ctx, a)
	if err != nil {
		return model.ConnTestResult{}, err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	r := bufio.NewReader(c)
	cmd := func(args ...string) (string, error) {
		var b strings.Builder
		fmt.Fprintf(&b, "*%d\r\n", len(args))
		for _, x := range args {
			fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(x), x)
		}
		if _, err := c.Write([]byte(b.String())); err != nil {
			return "", err
		}
		line, err := r.ReadString('\n')
		return strings.TrimSpace(line), err
	}
	if in.Password != "" {
		args := []string{"AUTH", in.Password}
		if in.User != "" {
			args = []string{"AUTH", in.User, in.Password}
		}
		if resp, err := cmd(args...); err != nil || !strings.HasPrefix(resp, "+") {
			return model.ConnTestResult{}, fmt.Errorf("AUTH failed: %s %v", resp, err)
		}
	}
	resp, err := cmd("PING")
	if err != nil {
		return model.ConnTestResult{}, err
	}
	if resp != "+PONG" {
		return model.ConnTestResult{}, fmt.Errorf("unexpected PING reply %q", resp)
	}
	return model.ConnTestResult{Message: "Connected · Redis PING OK"}, nil
}

func kafka(ctx context.Context, in model.ConnTestInput) (model.ConnTestResult, error) {
	var seeds []string
	for _, s := range strings.Split(in.Endpoint, ",") {
		if s = strings.TrimSpace(s); s != "" {
			seeds = append(seeds, s)
		}
	}
	if len(seeds) == 0 {
		return model.ConnTestResult{}, fmt.Errorf("endpoint (bootstrap servers) is required")
	}
	opts := []kgo.Opt{kgo.SeedBrokers(seeds...), kgo.DialTimeout(Timeout), kgo.RequestRetries(0)}
	if in.TLS {
		opts = append(opts, kgo.DialTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12}))
	}
	if in.User != "" {
		opts = append(opts, kgo.SASL(plain.Auth{User: in.User, Pass: in.Password}.AsMechanism()))
	}
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return model.ConnTestResult{}, err
	}
	defer cl.Close()
	adm := kadm.NewClient(cl)
	md, err := adm.Metadata(ctx)
	if err != nil {
		return model.ConnTestResult{}, fmt.Errorf("metadata request failed: %w", err)
	}
	groups, gerr := adm.ListGroups(ctx)
	msg := fmt.Sprintf("Connected · %d brokers · %d topics", len(md.Brokers), len(md.Topics.Names()))
	if gerr == nil {
		msg += fmt.Sprintf(" · %d consumer groups readable", len(groups))
	} else {
		msg += " · consumer groups not readable (Describe ACL missing?)"
	}
	return model.ConnTestResult{Message: msg}, nil
}

func rabbit(ctx context.Context, in model.ConnTestInput) (model.ConnTestResult, error) {
	if in.Endpoint == "" {
		return model.ConnTestResult{}, fmt.Errorf("endpoint (management API URL) is required")
	}
	base := strings.TrimRight(in.Endpoint, "/")
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/overview", nil)
	if err != nil {
		return model.ConnTestResult{}, err
	}
	if in.User != "" {
		req.SetBasicAuth(in.User, in.Password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return model.ConnTestResult{}, fmt.Errorf("management API unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return model.ConnTestResult{}, fmt.Errorf("management API answered %s (user needs the monitoring tag)", resp.Status)
	}
	var ov struct {
		Version string `json:"rabbitmq_version"`
		Cluster string `json:"cluster_name"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ov)
	return model.ConnTestResult{Message: "Connected · RabbitMQ " + ov.Version + " · management API readable", Version: ov.Version}, nil
}
