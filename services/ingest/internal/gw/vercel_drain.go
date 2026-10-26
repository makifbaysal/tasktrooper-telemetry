package gw

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tasktrooper/telemetry/services/ingest/internal/auth"
	"github.com/tasktrooper/telemetry/services/ingest/internal/obs"
	"github.com/tasktrooper/telemetry/services/ingest/internal/vercel"
)

// SetIntegrations enables /v1/drains/vercel.
func (s *Server) SetIntegrations(in Integrations) { s.integrations = in }

var (
	errNoIntegration = &ingestError{status: http.StatusNotFound, msg: "unknown or disabled integration", authReason: "integration"}
	errWrongProject  = &ingestError{status: http.StatusForbidden, msg: "integration belongs to another project", authReason: "integration"}
	errNoSecret      = &ingestError{status: http.StatusUnauthorized, msg: "drain secret not configured (integrations.config.drainSecretRef)", authReason: "signature"}
	errBadSignature  = &ingestError{status: http.StatusForbidden, msg: "signature didn't match", authReason: "signature"}
)

// vercelDrain serves POST /v1/drains/vercel?key=<ingest key>&integration=<id> (contracts §10).
//
//   - GET/HEAD, or a POST with an empty body / x-vercel-verify header and no signature, is Vercel's endpoint
//     verification: 200 with `x-vercel-verify: <config.verifyToken>`.
//   - Everything else must carry x-vercel-signature = hex(HMAC-SHA1(drain secret, body)). The signature is checked
//     against the decompressed body and, for gzip requests, also against the bytes as sent.
//   - Logs → tt.otlp.logs, traces (OTLP JSON or protobuf) → tt.otlp.traces, speed insights / web analytics → tt.rum.
func (s *Server) vercelDrain() http.Handler {
	return instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.handleVercel(w, r); err != nil {
			s.writeError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
}

func (s *Server) handleVercel(w http.ResponseWriter, r *http.Request) error {
	switch r.Method {
	case http.MethodPost, http.MethodGet, http.MethodHead:
	default:
		w.Header().Set("Allow", "POST, GET, HEAD")
		return &ingestError{status: http.StatusMethodNotAllowed, msg: "method not allowed"}
	}
	if s.integrations == nil {
		return &ingestError{status: http.StatusServiceUnavailable, msg: "drains are not enabled on this gateway"}
	}
	key, err := s.keys.Lookup(r.Context(), requestKey(r, true))
	if err != nil {
		if errors.Is(err, auth.ErrInvalidKey) {
			return errUnauthorized
		}
		s.log.Error("key lookup failed", "err", err)
		return &ingestError{status: http.StatusServiceUnavailable, msg: "auth backend unavailable", retryAfter: 5}
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("integration"), 10, 64)
	if err != nil || id <= 0 {
		return &ingestError{status: http.StatusBadRequest, msg: "integration query parameter required"}
	}
	in, err := s.integrations.Vercel(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNoIntegration) {
			return errNoIntegration
		}
		s.log.Error("integration lookup failed", "integration", id, "err", err)
		return &ingestError{status: http.StatusServiceUnavailable, msg: "integration backend unavailable", retryAfter: 5}
	}
	if in.ProjectID != int64(key.Tenant.ProjectID) {
		return errWrongProject
	}
	if in.Verify != "" {
		w.Header().Set("x-vercel-verify", in.Verify)
	}
	if r.Method != http.MethodPost {
		return nil // endpoint verification
	}
	sig := r.Header.Get("x-vercel-signature")
	wire, body, err := readBodyWire(r, s.cfg.MaxBodyBytes)
	switch {
	case errors.Is(err, errTooLarge):
		return &ingestError{status: http.StatusRequestEntityTooLarge, msg: "body exceeds " + strconv.FormatInt(s.cfg.MaxBodyBytes, 10) + " bytes"}
	case errors.Is(err, errBadEncoding):
		return &ingestError{status: http.StatusUnsupportedMediaType, msg: err.Error()}
	case err != nil:
		return &ingestError{status: http.StatusBadRequest, msg: "read body: " + err.Error()}
	}
	if sig == "" && (len(strings.TrimSpace(string(body))) == 0 || r.Header.Get("x-vercel-verify") != "") {
		return nil // verification POST
	}
	if len(in.Secret) == 0 {
		return errNoSecret
	}
	if !vercel.VerifySignature(in.Secret, body, sig) && (len(wire) == len(body) || !vercel.VerifySignature(in.Secret, wire, sig)) {
		return errBadSignature
	}
	if err := s.allow(r.Context(), key); err != nil {
		return err
	}

	var res *vercel.Result
	if !isJSON(r.Header.Get("Content-Type"), true) { // OTLP/HTTP protobuf trace drain
		var req coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			return &ingestError{status: http.StatusBadRequest, msg: "invalid protobuf trace payload: " + err.Error()}
		}
		res = vercel.ConvertTraceProto(in, &req)
	} else {
		res, err = vercel.Convert(in, body)
		if errors.Is(err, vercel.ErrEmpty) {
			return nil
		}
		if err != nil {
			return &ingestError{status: http.StatusBadRequest, msg: err.Error()}
		}
	}
	for kind, n := range res.Counts {
		obs.GWDrainEvents.WithLabelValues("vercel", kind).Add(float64(n))
	}
	// Drains come from Vercel's servers: client_ip / user_agent would describe Vercel, not the end user, so they
	// are not forwarded (country comes from the drain's own geo.country attribute).
	m := meta{tenant: key.Tenant, receivedAt: s.now()}
	if res.Logs != nil {
		if err := s.publishMsg(sigLogs, res.Logs, m); err != nil {
			return err
		}
	}
	for _, t := range res.Traces {
		if err := s.publishMsg(sigTraces, t, m); err != nil {
			return err
		}
	}
	for _, b := range res.RUM {
		if err := s.publishMsg(sigRUM, b, m); err != nil {
			return err
		}
	}
	return nil
}
