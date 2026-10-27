//go:build windows

package infra

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
)

// DefaultDockerSocket is TT_AGENT_DOCKER_SOCKET's default on Windows: the engine's named pipe.
const DefaultDockerSocket = "//./pipe/docker_engine"

// pipePath turns "//./pipe/x" or "npipe:////./pipe/x" into `\\.\pipe\x`.
func pipePath(s string) string {
	s = strings.TrimPrefix(s, "npipe:")
	return strings.ReplaceAll(s, "/", `\`)
}

// dialEngine connects to the engine over a named pipe (or a unix socket path, which Windows
// supports too, when the path is not a pipe).
func dialEngine(ctx context.Context, socket string) (net.Conn, error) {
	p := pipePath(socket)
	if strings.HasPrefix(p, `\\.\pipe\`) {
		return winio.DialPipeContext(ctx, p)
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", socket)
}

// engineAvailable probes the pipe (a named pipe cannot be stat'ed reliably).
func engineAvailable(socket string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	c, err := dialEngine(ctx, socket)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
