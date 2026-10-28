package gw

import (
	"bytes"
	"compress/gzip"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tasktrooper/telemetry/services/ingest/internal/auth"
	"github.com/tasktrooper/telemetry/services/ingest/internal/profiling"
	"github.com/tasktrooper/telemetry/services/ingest/internal/replay"
	"github.com/tasktrooper/telemetry/services/pkg/topics"
)

// ── POST /v1/replay (contracts §23) ──
// Auth, CORS, ?key= and content types as /v1/rum; the body is one segment JSON. It is validated and published gzip'ed.

func (s *Server) replayEndpoint() http.Handler {
	return instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && corsHeaders(w, r, origin) {
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := s.handleReplay(r, origin); err != nil {
			if errors.Is(err, errForbidden) {
				w.Header().Del("Access-Control-Allow-Origin")
			}
			s.writeError(w, err)
			return
		}
		writeJSONOK(w)
	}))
}

func (s *Server) handleReplay(r *http.Request, origin string) error {
	key, err := s.authenticate(r.Context(), strings.TrimSpace(r.Header.Get(auth.HeaderInternal)), r.Header.Get(auth.HeaderProject),
		origin, requestKey(r, true))
	if err != nil {
		return err
	}
	if err := s.allow(r.Context(), key); err != nil {
		return err
	}
	wire, body, err := readBodyLimits(r, s.cfg.ReplayMaxWireBytes, s.cfg.ReplayMaxBodyBytes)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			return &ingestError{status: http.StatusRequestEntityTooLarge, msg: "replay segment exceeds " +
				strconv.FormatInt(s.cfg.ReplayMaxWireBytes, 10) + " bytes compressed / " + strconv.FormatInt(s.cfg.ReplayMaxBodyBytes, 10) + " bytes"}
		}
		return bodyError(err, s.cfg.ReplayMaxBodyBytes)
	}
	seg, err := replay.Decode(body)
	if err != nil {
		return &ingestError{status: http.StatusBadRequest, msg: "invalid replay segment: " + err.Error()}
	}
	value := wire
	if !profiling.IsGzip(wire) || len(wire) == len(body) && bytes.Equal(wire, body) {
		if value, err = gzipBytes(body); err != nil {
			return &ingestError{status: http.StatusInternalServerError, msg: "compress: " + err.Error()}
		}
	}
	if len(value) > topics.MaxLargeRecordBytes {
		return &ingestError{status: http.StatusRequestEntityTooLarge, msg: "replay segment too large"}
	}
	m := meta{tenant: key.Tenant, receivedAt: s.now(), clientIP: clientIP(r)}
	return s.send(topics.Replay, [][]byte{value}, m,
		kgo.RecordHeader{Key: topics.HeaderSessionID, Value: []byte(seg.SessionID)},
		kgo.RecordHeader{Key: topics.HeaderSegment, Value: headerValue(*seg.Segment)})
}

var gzwPool = sync.Pool{New: func() any { w, _ := gzip.NewWriterLevel(nil, gzip.DefaultCompression); return w }}

func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(b)/4 + 64)
	zw := gzwPool.Get().(*gzip.Writer)
	defer gzwPool.Put(zw)
	zw.Reset(&buf)
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeJSONOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("{}"))
}

// ── POST /v1/profiles (contracts §24) ──
// Server-side SDKs only: x-trooper-key / Bearer / internal credential; no ?key=, no CORS, browser requests (Origin) → 403.

var errProfilesBrowser = &ingestError{status: http.StatusForbidden, msg: "profiles are not accepted from browsers", authReason: "origin"}

var validRuntimes = map[string]bool{"": true, "go": true, "jvm": true, "node": true, "dotnet": true, "python": true}

const maxProfileLabel = 256

// profileParams are the validated query parameters.
type profileParams struct {
	service, env, version, host, runtime string
	start, end                           int64
	format, typ, unit                    string
}

func parseProfileParams(r *http.Request) (*profileParams, error) {
	q := r.URL.Query()
	p := &profileParams{
		service: strings.TrimSpace(q.Get("service")), env: q.Get("env"), version: q.Get("version"), host: q.Get("host"),
		runtime: strings.ToLower(q.Get("runtime")), format: strings.ToLower(q.Get("format")), typ: q.Get("type"), unit: q.Get("unit"),
	}
	if p.service == "" {
		return nil, errors.New("service is required")
	}
	for _, v := range []string{p.service, p.env, p.version, p.host} {
		if len(v) > maxProfileLabel {
			return nil, errors.New("service / env / version / host must be ≤ 256 bytes")
		}
	}
	if !validRuntimes[p.runtime] {
		return nil, errors.New("runtime must be go | jvm | node | dotnet | python")
	}
	var err error
	if p.start, err = strconv.ParseInt(q.Get("start"), 10, 64); err != nil || p.start <= 0 {
		return nil, errors.New("start (unix ms) is required")
	}
	if p.end, err = strconv.ParseInt(q.Get("end"), 10, 64); err != nil || p.end <= 0 {
		return nil, errors.New("end (unix ms) is required")
	}
	if p.end < p.start {
		return nil, errors.New("end must be ≥ start")
	}
	if p.end-p.start > 3_600_000 {
		return nil, errors.New("a profile may cover at most 1 hour")
	}
	if p.typ != "" && !profiling.ValidType(p.typ) {
		return nil, errors.New("unknown type " + strconv.Quote(p.typ))
	}
	switch p.format {
	case "":
		p.format = profiling.FormatPprof
	case profiling.FormatPprof:
	case profiling.FormatFolded:
		if p.typ == "" || p.unit == "" {
			return nil, errors.New("folded profiles need type and unit")
		}
	default:
		return nil, errors.New("format must be pprof or folded")
	}
	if p.unit != "" && p.typ != "" && p.unit != profiling.DefaultUnit(p.typ) {
		return nil, errors.New("unit of " + p.typ + " must be " + profiling.DefaultUnit(p.typ))
	}
	return p, nil
}

func (s *Server) profilesEndpoint() http.Handler {
	return instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := s.handleProfile(r); err != nil {
			s.writeError(w, err)
			return
		}
		writeJSONOK(w)
	}))
}

func (s *Server) handleProfile(r *http.Request) error {
	if r.Header.Get("Origin") != "" {
		return errProfilesBrowser
	}
	key, err := s.authenticate(r.Context(), strings.TrimSpace(r.Header.Get(auth.HeaderInternal)), r.Header.Get(auth.HeaderProject),
		"", requestKey(r, false))
	if err != nil {
		return err
	}
	if err := s.allow(r.Context(), key); err != nil {
		return err
	}
	p, err := parseProfileParams(r)
	if err != nil {
		return &ingestError{status: http.StatusBadRequest, msg: err.Error()}
	}
	wire, body, err := readBodyLimits(r, s.cfg.ProfileMaxWireBytes, s.cfg.ProfileMaxBodyBytes)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			return &ingestError{status: http.StatusRequestEntityTooLarge, msg: "profile exceeds " +
				strconv.FormatInt(s.cfg.ProfileMaxWireBytes, 10) + " bytes / " + strconv.FormatInt(s.cfg.ProfileMaxBodyBytes, 10) + " bytes decompressed"}
		}
		return bodyError(err, s.cfg.ProfileMaxWireBytes)
	}
	if len(body) == 0 {
		return &ingestError{status: http.StatusBadRequest, msg: "empty profile"}
	}
	// Validate by parsing (profiles are low-rate: one per type and minute per instance).
	if _, err := profiling.Parse(body, profiling.Options{Format: p.format, Type: p.typ, Unit: p.unit, Runtime: p.runtime,
		MaxSize: s.cfg.ProfileMaxBodyBytes}); err != nil {
		if errors.Is(err, profiling.ErrTooLarge) {
			return &ingestError{status: http.StatusRequestEntityTooLarge, msg: "profile exceeds " + strconv.FormatInt(s.cfg.ProfileMaxBodyBytes, 10) + " bytes decompressed"}
		}
		return &ingestError{status: http.StatusBadRequest, msg: "invalid profile: " + err.Error()}
	}
	// Publish the smaller form: the gzip'ed wire bytes when the request was Content-Encoding: gzip (the writer detects the
	// gzip magic), the body as received otherwise (pprof is usually gzip'ed itself).
	value := body
	if len(wire) < len(body) && profiling.IsGzip(wire) {
		value = wire
	}
	m := meta{tenant: key.Tenant, receivedAt: s.now(), clientIP: clientIP(r)}
	h := func(k, v string) kgo.RecordHeader { return kgo.RecordHeader{Key: k, Value: []byte(v)} }
	return s.send(topics.Profiles, [][]byte{value}, m,
		h(topics.HeaderService, p.service), h(topics.HeaderEnv, p.env), h(topics.HeaderVersion, p.version),
		h(topics.HeaderHost, p.host), h(topics.HeaderRuntime, p.runtime),
		h(topics.HeaderStart, strconv.FormatInt(p.start, 10)), h(topics.HeaderEnd, strconv.FormatInt(p.end, 10)),
		h(topics.HeaderFormat, p.format), h(topics.HeaderType, p.typ), h(topics.HeaderUnit, p.unit))
}
