// Package geotest builds tiny GeoLite2-Country-shaped mmdb files for tests (no real database is bundled).
package geotest

import (
	"net"
	"os"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

// Write creates an mmdb at path mapping each CIDR to an ISO country code.
func Write(path string, nets map[string]string) error {
	w, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: "GeoLite2-Country", RecordSize: 24, IncludeReservedNetworks: true})
	if err != nil {
		return err
	}
	for cidr, cc := range nets {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			return err
		}
		if err := w.Insert(n, mmdbtype.Map{"country": mmdbtype.Map{"iso_code": mmdbtype.String(cc)}}); err != nil {
			return err
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := w.WriteTo(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
