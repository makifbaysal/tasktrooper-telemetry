// Package orch supervises the demo's processes: the platform (ingest-gw, ingest-writer, api,
// alert-engine, notifier — started only when not already running), one OS process per simulated
// service instance, and the collector (trooper-agent) with a generated config.
package orch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tasktrooper/telemetry/tools/demo/internal/world"
)

// Root finds the repository root (the directory holding services/go.work and tools/demo).
func Root() (string, error) {
	dir, _ := os.Getwd()
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "services", "go.work")); err == nil {
			return d, nil
		}
		if filepath.Dir(d) == d {
			return "", errors.New("run inside the tasktrooper-telemetry repository")
		}
	}
}

// Proc is a supervised child process.
type Proc struct {
	Name string
	Cmd  *exec.Cmd
	Log  string
	done chan struct{}
	err  error
}

// Supervisor starts processes and stops them in reverse order.
type Supervisor struct {
	Root, State string
	Log         *slog.Logger
	procs       []*Proc
}

func NewSupervisor(root string, log *slog.Logger) (*Supervisor, error) {
	s := &Supervisor{Root: root, State: filepath.Join(root, "tools", "demo", ".run"), Log: log}
	for _, d := range []string{"logs", "bin"} {
		if err := os.MkdirAll(filepath.Join(s.State, d), 0o755); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Start runs bin with args/env; output goes to .run/logs/<name>.log.
func (s *Supervisor) Start(name, dir, bin string, args, env []string) (*Proc, error) {
	logPath := filepath.Join(s.State, "logs", name+".log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // Ctrl-C reaches only us; we stop children in order
	if err := cmd.Start(); err != nil {
		f.Close()
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	p := &Proc{Name: name, Cmd: cmd, Log: logPath, done: make(chan struct{})}
	go func() { p.err = cmd.Wait(); f.Close(); close(p.done) }()
	s.procs = append(s.procs, p)
	return p, nil
}

// Exited reports a process that stopped on its own.
func (s *Supervisor) Exited() *Proc {
	for _, p := range s.procs {
		select {
		case <-p.done:
			return p
		default:
		}
	}
	return nil
}

// StopAll stops processes in reverse start order (SIGTERM, then SIGKILL after 20 s).
func (s *Supervisor) StopAll() {
	for i := len(s.procs) - 1; i >= 0; i-- {
		s.stop(s.procs[i])
	}
	s.procs = nil
}

// StopWhere stops matching processes (all at once) and waits for them.
func (s *Supervisor) StopWhere(match func(*Proc) bool) {
	var keep, stop []*Proc
	for _, p := range s.procs {
		if match(p) {
			stop = append(stop, p)
			_ = syscall.Kill(-p.Cmd.Process.Pid, syscall.SIGTERM)
		} else {
			keep = append(keep, p)
		}
	}
	for _, p := range stop {
		s.wait(p)
	}
	s.procs = keep
}

func (s *Supervisor) stop(p *Proc) {
	_ = syscall.Kill(-p.Cmd.Process.Pid, syscall.SIGTERM)
	s.wait(p)
}

func (s *Supervisor) wait(p *Proc) {
	select {
	case <-p.done:
	case <-time.After(20 * time.Second):
		_ = syscall.Kill(-p.Cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
	}
}

// ---------------------------------------------------------------- builds

// Build compiles a Go command of a services/ module (GOWORK=off, like the services' READMEs).
func (s *Supervisor) Build(ctx context.Context, module, pkg, out string) (string, error) {
	bin := filepath.Join(s.State, "bin", out)
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, pkg)
	cmd.Dir = filepath.Join(s.Root, "services", module)
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build %s %s: %v\n%s", module, pkg, err, b)
	}
	return bin, nil
}

// ---------------------------------------------------------------- platform

type platformSvc struct {
	name, module, pkg, health string
}

var platform = []platformSvc{
	{"ingest-writer", "ingest", "./cmd/ingest-writer", "http://localhost:4320/healthz"},
	{"ingest-gw", "ingest", "./cmd/ingest-gw", "http://localhost:4318/healthz"},
	{"api", "api", "./cmd/api", "http://localhost:8080/healthz"},
	{"alert-engine", "alerting", "./cmd/alert-engine", "http://localhost:8090/healthz"},
	{"notifier", "alerting", "./cmd/notifier", "http://localhost:8091/healthz"},
}

// Healthy reports whether url answers 2xx.
func Healthy(url string) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 300
}

// Platform starts the platform services that are not running yet and waits for them.
func (s *Supervisor) Platform(ctx context.Context) error {
	for _, p := range platform {
		if Healthy(p.health) {
			s.Log.Info("platform service already running", "name", p.name)
			continue
		}
		s.Log.Info("building", "name", p.name)
		bin, err := s.Build(ctx, p.module, p.pkg, p.name)
		if err != nil {
			return err
		}
		if _, err := s.Start("platform-"+p.name, filepath.Join(s.Root, "services", p.module), bin, nil, s3Env()); err != nil {
			return err
		}
		deadline := time.Now().Add(60 * time.Second)
		for !Healthy(p.health) {
			if time.Now().After(deadline) {
				return fmt.Errorf("%s did not become healthy (log: %s)", p.name, filepath.Join(s.State, "logs", "platform-"+p.name+".log"))
			}
			if pr := s.Exited(); pr != nil {
				return fmt.Errorf("%s exited: %v (log: %s)", pr.Name, pr.err, pr.Log)
			}
			time.Sleep(500 * time.Millisecond)
		}
		s.Log.Info("platform service started", "name", p.name)
	}
	return nil
}

// ---------------------------------------------------------------- collector

// CollectorConfig writes the trooper-agent static config for the ttdemo containers.
func (s *Supervisor) CollectorConfig() (string, []string) {
	var b strings.Builder
	b.WriteString("# generated by `demo run` — trooper-agent targets for the ttdemo containers\ninterval: 15s\ntimeout: 10s\ndatabases:\n")
	for _, p := range world.PGs {
		fmt.Fprintf(&b, "  - {id: %s, name: %s, engine: postgres, host: 127.0.0.1, port: %d, collector: agent, user: postgres, database: postgres, tls: disable, credentialsRef: env:TTDEMO_PG_PASSWORD, node: %s-0, slowQueryMs: 200}\n",
			p.ID, p.ID, p.Port, p.ID)
	}
	for _, r := range world.Redises {
		fmt.Fprintf(&b, "  - {id: %s, name: %s, engine: redis, host: 127.0.0.1, port: %d, collector: agent, dsn: \"redis://127.0.0.1:%d/0\", node: %s-0001}\n",
			r.ID, r.ID, r.Port, r.Port, r.ID)
	}
	fmt.Fprintf(&b, "  - {id: notifications-db, name: notifications-db, engine: mysql, host: 127.0.0.1, port: %d, collector: direct, user: root, database: notifications, credentialsRef: env:TTDEMO_MYSQL_PASSWORD, node: notifications-db}\n", world.MySQLPort)
	b.WriteString("brokerClusters:\n")
	fmt.Fprintf(&b, "  - {id: kafka-main, name: kafka-main, kind: kafka, endpoint: \"%s\", collector: agent, auth: {mechanism: none}, groupFilter: '%s', topicFilter: '%s'}\n",
		world.DefaultKafka, world.GroupFilter, world.TopicFilter)
	fmt.Fprintf(&b, "  - {id: rabbit-main, name: rabbit-main, kind: rabbitmq, endpoint: \"http://127.0.0.1:%d\", vhost: \"%s\", collector: agent, user: %s, credentialsRef: env:TTDEMO_RABBIT_PASSWORD}\n",
		world.RabbitMgmt, world.RabbitVhost, world.RabbitUser)
	path := filepath.Join(s.State, "collector.yaml")
	_ = os.WriteFile(path, []byte(b.String()), 0o644)
	return path, []string{"TTDEMO_PG_PASSWORD=" + world.PGPassword, "TTDEMO_MYSQL_PASSWORD=" + world.MySQLPassword, "TTDEMO_RABBIT_PASSWORD=" + world.RabbitPass}
}

// FreePort returns a free local TCP port.
func FreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// s3Env points the platform services at the compose S3 (SeaweedFS on :9002) for session replay blobs
// (contracts §23: without TT_S3_ENDPOINT the writer skips tt.replay and the api answers 503 for
// segments). Values already set in the environment win.
func s3Env() []string {
	var out []string
	for _, kv := range [][2]string{
		{"TT_S3_ENDPOINT", "http://localhost:9002"},
		{"TT_S3_ACCESS_KEY", "trooper"},
		{"TT_S3_SECRET_KEY", "trooper-secret"},
		{"TT_REPLAY_S3_BUCKET", "tt-replays"},
	} {
		if os.Getenv(kv[0]) == "" {
			out = append(out, kv[0]+"="+kv[1])
		}
	}
	return out
}

// WriterReplayDisabled reports whether a running ingest-writer exposes tt_writer_replay_disabled 1 on
// its metrics port (:9464), i.e. it was started without TT_S3_ENDPOINT and drops replay segments.
func WriterReplayDisabled() bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://localhost:9464/metrics")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "tt_writer_replay_disabled") && strings.HasSuffix(strings.TrimSpace(l), " 1") {
			return true
		}
	}
	return false
}
