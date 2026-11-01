package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func emptyEnvFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mcp.env")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseEnvFile(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr bool
	}{
		{name: "plain", in: "TT_API_URL=https://api.example.com\nTT_API_KEY=tt_api_x\n",
			want: map[string]string{"TT_API_URL": "https://api.example.com", "TT_API_KEY": "tt_api_x"}},
		{name: "comments, export, quotes", in: "# installer\n\nexport TT_API_KEY=\"tt_api_y\"\nTT_MCP_ALLOW_WRITE='true'\n",
			want: map[string]string{"TT_API_KEY": "tt_api_y", "TT_MCP_ALLOW_WRITE": "true"}},
		{name: "unrelated keys ignored", in: "PATH=/evil\nTT_LOG_LEVEL=debug\n", want: map[string]string{"TT_LOG_LEVEL": "debug"}},
		{name: "not key=value", in: "TT_API_KEY\n", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseEnvFile(strings.NewReader(tc.in))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// The environment wins over the file; the file fills what is unset; a readable-by-others file is flagged.
func TestLoadEnvFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mcp.env")
	if err := os.WriteFile(p, []byte("TT_API_URL=https://file.example.com\nTT_API_KEY=tt_api_file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TT_MCP_ENV_FILE", p)
	t.Setenv("TT_API_URL", "https://env.example.com")
	_ = os.Unsetenv("TT_API_KEY")
	t.Cleanup(func() { _ = os.Unsetenv("TT_API_KEY") })

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.APIURL != "https://env.example.com" || c.APIKey != "tt_api_file" || c.EnvFile != p {
		t.Errorf("config = %+v", c)
	}
	if !strings.Contains(c.EnvWarning, "chmod 600") {
		t.Errorf("warning = %q, want a chmod hint", c.EnvWarning)
	}

	t.Setenv("TT_MCP_ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	if _, err := Load(); err == nil {
		t.Error("a named env file that does not exist must be an error")
	}
}
