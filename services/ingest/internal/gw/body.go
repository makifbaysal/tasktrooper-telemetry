package gw

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
)

var (
	errTooLarge    = errors.New("request body too large")
	errBadEncoding = errors.New("unsupported content-encoding")
)

var gzPool sync.Pool

// readBody reads the request body, transparently gunzipping it, and enforces max on both the wire
// size and the decompressed size (zip-bomb guard).
func readBody(r *http.Request, max int64) ([]byte, error) {
	_, body, err := readBodyWire(r, max)
	return body, err
}

// readBodyWire is readBody that also returns the bytes as received (before decompression).
func readBodyWire(r *http.Request, max int64) (wire, body []byte, err error) {
	if r.ContentLength > max {
		return nil, nil, errTooLarge
	}
	raw, err := readLimited(r.Body, max, r.ContentLength)
	if err != nil {
		return nil, nil, err
	}
	switch enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
		return raw, raw, nil
	case "gzip", "x-gzip":
		var zr *gzip.Reader
		if v := gzPool.Get(); v != nil {
			zr = v.(*gzip.Reader)
			err = zr.Reset(bytes.NewReader(raw))
		} else {
			zr, err = gzip.NewReader(bytes.NewReader(raw))
		}
		if err != nil {
			return nil, nil, err
		}
		defer gzPool.Put(zr)
		body, err = readLimited(zr, max, int64(len(raw))*4)
		return raw, body, err
	default:
		return nil, nil, errBadEncoding
	}
}

// readLimited reads at most max bytes from rd; more than max → errTooLarge.
func readLimited(rd io.Reader, max, hint int64) ([]byte, error) {
	if hint <= 0 || hint > max {
		hint = min(max, 64<<10)
	}
	buf := bytes.NewBuffer(make([]byte, 0, hint+1))
	n, err := buf.ReadFrom(io.LimitReader(rd, max+1))
	if err != nil {
		return nil, err
	}
	if n > max {
		return nil, errTooLarge
	}
	return buf.Bytes(), nil
}
