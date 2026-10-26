// Package geo resolves client IPs to ISO country codes with a MaxMind GeoLite2-Country / DB-IP Lite
// (mmdb) database. No database is bundled: set TT_GEOIP_DB to a downloaded .mmdb file.
package geo

import (
	"net/netip"
	"strings"

	"github.com/oschwald/maxminddb-golang/v2"
)

// DB looks up countries. A nil *DB is valid and resolves nothing.
type DB struct{ r *maxminddb.Reader }

// Open opens an mmdb file. GeoLite2-City / DB-IP City files work too (same country record).
func Open(path string) (*DB, error) {
	r, err := maxminddb.Open(path)
	if err != nil {
		return nil, err
	}
	return &DB{r: r}, nil
}

// Close releases the database.
func (d *DB) Close() error {
	if d == nil || d.r == nil {
		return nil
	}
	return d.r.Close()
}

type record struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	RegisteredCountry struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"registered_country"`
}

// Country returns the ISO 3166-1 alpha-2 code for ip ("" if unknown, private or unparsable).
func (d *DB) Country(ip string) string {
	if d == nil || d.r == nil || ip == "" {
		return ""
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return ""
	}
	addr = addr.Unmap()
	if addr.IsPrivate() || addr.IsLoopback() || addr.IsUnspecified() || addr.IsLinkLocalUnicast() {
		return ""
	}
	var rec record
	if err := d.r.Lookup(addr).Decode(&rec); err != nil {
		return ""
	}
	if rec.Country.ISOCode != "" {
		return rec.Country.ISOCode
	}
	return rec.RegisteredCountry.ISOCode
}
