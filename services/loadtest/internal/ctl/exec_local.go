package ctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Job is one region runner of a run.
type Job struct {
	RunID     int64
	ProjectID int64
	Region    string
	Share     float64
	VUCap     int
	// Deadline bounds the runner (k8s activeDeadlineSeconds).
	Deadline time.Duration
}

// Env returns the runner environment specific to the job (contracts §14 runner config).
func (j Job) Env() map[string]string {
	return map[string]string{
		"TT_LOADTEST_RUN_ID": strconv.FormatInt(j.RunID, 10),
		"TT_LOADTEST_REGION": j.Region,
		"TT_LOADTEST_SHARE":  strconv.FormatFloat(j.Share, 'f', -1, 64),
		"TT_LOADTEST_VU_CAP": strconv.Itoa(j.VUCap),
	}
}

// Executor starts runners.
type Executor interface {
	Start(ctx context.Context, j Job) (Handle, error)
}

// Handle controls a started runner.
type Handle interface {
	// Stop asks the runner to stop (it writes its final shard row on the way out).
	Stop(ctx context.Context) error
	// Status reports whether the runner exited, and its failure if any.
	Status(ctx context.Context) (exited bool, err error)
}

// LocalExecutor spawns loadtest-runner subprocesses (TT_LOADTEST_EXECUTOR=local).
type LocalExecutor struct {
	Bin string            // runner binary
	Env map[string]string // shared runner env (DB, ingest, log level)
}

// FindRunner locates loadtest-runner: explicit path, next to the running binary, or on PATH.
func FindRunner(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if self, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(self), "loadtest-runner")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return exec.LookPath("loadtest-runner")
}

type localHandle struct {
	cmd  *exec.Cmd
	mu   sync.Mutex
	done bool
	err  error
}

// Start spawns the runner.
func (l *LocalExecutor) Start(_ context.Context, j Job) (Handle, error) {
	if l.Bin == "" {
		return nil, errors.New("local executor: runner binary not found (TT_LOADTEST_RUNNER_BIN)")
	}
	cmd := exec.Command(l.Bin)
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.TempDir()}
	for k, v := range l.Env {
		env = append(env, k+"="+v)
	}
	for k, v := range j.Env() {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start runner: %w", err)
	}
	h := &localHandle{cmd: cmd}
	go func() {
		err := cmd.Wait()
		h.mu.Lock()
		h.done, h.err = true, err
		h.mu.Unlock()
	}()
	return h, nil
}

func (h *localHandle) Stop(_ context.Context) error {
	h.mu.Lock()
	done := h.done
	h.mu.Unlock()
	if done {
		return nil
	}
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	go func() { // hard kill if it does not exit
		time.Sleep(30 * time.Second)
		h.mu.Lock()
		d := h.done
		h.mu.Unlock()
		if !d {
			_ = h.cmd.Process.Kill()
		}
	}()
	return nil
}

func (h *localHandle) Status(_ context.Context) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.done, h.err
}
