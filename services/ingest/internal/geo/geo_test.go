package geo_test

import (
	"path/filepath"
	"testing"

	"github.com/tasktrooper/telemetry/services/ingest/internal/geo"
	"github.com/tasktrooper/telemetry/services/ingest/internal/geo/geotest"
)

func TestCountry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.mmdb")
	if err := geotest.Write(path, map[string]string{"81.214.0.0/16": "TR", "2a02:aa00::/24": "TR", "8.8.8.0/24": "US"}); err != nil {
		t.Fatal(err)
	}
	db, err := geo.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for ip, want := range map[string]string{
		"81.214.12.9": "TR", "8.8.8.8": "US", "2a02:aa10::1": "TR", "::ffff:8.8.8.8": "US",
		"1.1.1.1": "", "10.0.0.1": "", "127.0.0.1": "", "garbage": "", "": "",
	} {
		if got := db.Country(ip); got != want {
			t.Errorf("Country(%q)=%q want %q", ip, got, want)
		}
	}
	var nilDB *geo.DB
	if nilDB.Country("8.8.8.8") != "" {
		t.Error("nil DB must resolve nothing")
	}
}
