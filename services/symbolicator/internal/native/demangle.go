package native

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var (
	demanglerOnce sync.Once
	demanglerPath string
)

// SwiftDemanglerPath locates swift-demangle: $TT_SWIFT_DEMANGLE, $PATH, then `xcrun --find` (macOS).
// Empty when unavailable (e.g. the distroless container): names stay mangled, frames keep `mangled`.
func SwiftDemanglerPath() string {
	demanglerOnce.Do(func() {
		if p := os.Getenv("TT_SWIFT_DEMANGLE"); p != "" {
			if p == "off" {
				return
			}
			demanglerPath = p
			return
		}
		if p, err := exec.LookPath("swift-demangle"); err == nil {
			demanglerPath = p
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "xcrun", "--find", "swift-demangle").Output(); err == nil {
			demanglerPath = strings.TrimSpace(string(out))
		}
	})
	return demanglerPath
}

// DemangleSwift demangles names in one swift-demangle invocation (simplified form, e.g.
// "closure #1 in CheckoutService.checkout(_:)"). Returns an empty map when the tool is unavailable.
func DemangleSwift(names []string) map[string]string {
	out := map[string]string{}
	path := SwiftDemanglerPath()
	if path == "" || len(names) == 0 {
		return out
	}
	var in bytes.Buffer
	var order []string
	for _, n := range names {
		if strings.ContainsAny(n, "\n\r") {
			continue
		}
		order = append(order, n)
		in.WriteString(n)
		in.WriteByte('\n')
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--simplified")
	cmd.Stdin = &in
	res, err := cmd.Output()
	if err != nil {
		return out
	}
	lines := strings.Split(strings.TrimRight(string(res), "\n"), "\n")
	if len(lines) != len(order) {
		return out
	}
	for i, n := range order {
		if d := strings.TrimSpace(lines[i]); d != "" && d != n {
			out[n] = d
		}
	}
	return out
}
