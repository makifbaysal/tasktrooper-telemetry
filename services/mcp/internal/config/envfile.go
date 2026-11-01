package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// envFileKeys are the settings an env file may provide. Anything else in the file is ignored, so it can never
// change unrelated process state.
var envFileKeys = map[string]bool{
	"TT_API_URL": true, "TT_API_KEY": true, "TT_MCP_ALLOW_WRITE": true,
	"TT_ORG_ID": true, "TT_PROJECT_ID": true, "TT_LOG_LEVEL": true,
}

// LoadEnvFile fills unset TT_* settings from the first env file found, so coding-tool configs can start `tt-mcp`
// without the API key in them (the installer writes the file with mode 0600):
//
//  1. $TT_MCP_ENV_FILE
//  2. .tasktrooper/mcp.env in the working directory or a parent, up to the repository root (project install)
//  3. $XDG_CONFIG_HOME/tasktrooper/mcp.env, default ~/.config/tasktrooper/mcp.env (global install)
//
// Variables already set in the environment win. It returns the file used ("" when none) and a warning for a file
// other users can read; a missing file is not an error, an unreadable named one is.
func LoadEnvFile() (path, warning string, err error) {
	if p := os.Getenv("TT_MCP_ENV_FILE"); p != "" {
		return p, permWarning(p), applyEnvFile(p)
	}
	for _, p := range envFileCandidates() {
		if _, statErr := os.Stat(p); statErr == nil {
			return p, permWarning(p), applyEnvFile(p)
		}
	}
	return "", "", nil
}

func envFileCandidates() []string {
	var out []string
	if wd, err := os.Getwd(); err == nil {
		for dir := wd; ; dir = filepath.Dir(dir) {
			out = append(out, filepath.Join(dir, ".tasktrooper", "mcp.env"))
			if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil || filepath.Dir(dir) == dir {
				break
			}
		}
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(home, ".config")
		}
	}
	if base != "" {
		out = append(out, filepath.Join(base, "tasktrooper", "mcp.env"))
	}
	return out
}

func applyEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("TT_MCP_ENV_FILE %s: %w", path, err)
	}
	defer f.Close()
	vals, err := parseEnvFile(f)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for k, v := range vals {
		if _, set := os.LookupEnv(k); !set {
			_ = os.Setenv(k, v)
		}
	}
	return nil
}

// parseEnvFile reads KEY=VALUE lines (blank lines and # comments skipped, optional "export ", surrounding quotes
// stripped). Only envFileKeys are returned.
func parseEnvFile(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			return nil, fmt.Errorf("line %d is not KEY=VALUE", n)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if envFileKeys[k] {
			out[k] = v
		}
	}
	return out, sc.Err()
}

func permWarning(path string) string {
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm()&0o077 == 0 {
		return ""
	}
	return fmt.Sprintf("%s holds the API key but other users can read it (mode %o); run: chmod 600 %s", path, st.Mode().Perm(), path)
}
