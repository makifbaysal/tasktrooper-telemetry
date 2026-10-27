//go:build !windows

package infra

import (
	"context"
	"net"
	"os"
)

// DefaultDockerSocket is TT_AGENT_DOCKER_SOCKET's default.
const DefaultDockerSocket = "/var/run/docker.sock"

// dialEngine connects to a Docker-compatible engine API on a unix socket.
func dialEngine(ctx context.Context, socket string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", socket)
}

// engineAvailable reports whether socket is a unix socket.
func engineAvailable(socket string) bool {
	st, err := os.Stat(socket)
	return err == nil && st.Mode()&os.ModeSocket != 0
}
