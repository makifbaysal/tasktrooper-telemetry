package gw

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/twmb/franz-go/pkg/kgo"
	colprofilespb "go.opentelemetry.io/proto/otlp/collector/profiles/v1development"
	"google.golang.org/protobuf/proto"

	"github.com/tasktrooper/telemetry/services/ingest/internal/auth"
	"github.com/tasktrooper/telemetry/services/ingest/internal/profiling"
	"github.com/tasktrooper/telemetry/services/pkg/topics"
)

// ── OTLP profiles (contracts §24 "OTLP profiles") ──
// POST /v1development/profiles (OTLP/HTTP protobuf or JSON, gzip) and gRPC ProfilesService/Export: auth and rate limit as the
// other OTLP endpoints, body limits of /v1/profiles. The request is validated by decoding it and published as one tt.profiles
// record (format=otlp, value = ExportProfilesServiceRequest protobuf, gzip'ed when large); the writer reads the identity
// from the resource attributes, so the service / time headers are not set.

// OTLPProfilesPath is the OTLP/HTTP path of the profiles signal (otlphttp exporter: <endpoint>/v1development/profiles).
const OTLPProfilesPath = "/v1development/profiles"

// otlpProfileGzipAbove: values above this are gzip'ed before producing (the blob topics' producer does not compress).
const otlpProfileGzipAbove = 256 << 10

var sigProfiles = signal{topic: topics.Profiles,
	newMsg: func() proto.Message { return &colprofilespb.ExportProfilesServiceRequest{} }}

func (s *Server) otlpProfilesEndpoint() http.Handler {
	return instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		jsonReq := isJSON(r.Header.Get("Content-Type"), false)
		if err := s.handleOTLPProfiles(r, jsonReq); err != nil {
			s.writeError(w, err)
			return
		}
		if jsonReq {
			writeJSONOK(w)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf") // empty ExportProfilesServiceResponse
		w.WriteHeader(http.StatusOK)
	}))
}

func (s *Server) handleOTLPProfiles(r *http.Request, jsonReq bool) error {
	key, err := s.authenticate(r.Context(), strings.TrimSpace(r.Header.Get(auth.HeaderInternal)), r.Header.Get(auth.HeaderProject),
		r.Header.Get("Origin"), requestKey(r, false))
	if err != nil {
		return err
	}
	if err := s.allow(r.Context(), key); err != nil {
		return err
	}
	wire, body, err := readBodyLimits(r, s.cfg.ProfileMaxWireBytes, s.cfg.ProfileMaxBodyBytes)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			return &ingestError{status: http.StatusRequestEntityTooLarge, msg: "profiles request exceeds " +
				strconv.FormatInt(s.cfg.ProfileMaxWireBytes, 10) + " bytes / " + strconv.FormatInt(s.cfg.ProfileMaxBodyBytes, 10) + " bytes decompressed"}
		}
		return bodyError(err, s.cfg.ProfileMaxWireBytes)
	}
	req := &colprofilespb.ExportProfilesServiceRequest{}
	if jsonReq {
		err = unmarshalOTLPJSON(body, req)
		body, wire = nil, nil // re-encoded as protobuf
	} else {
		err = proto.Unmarshal(body, req)
	}
	if err != nil {
		return &ingestError{status: http.StatusBadRequest, msg: "invalid payload: " + err.Error()}
	}
	m := meta{tenant: key.Tenant, receivedAt: s.now(), clientIP: clientIP(r)}
	return s.publishOTLPProfiles(req, body, wire, m)
}

// publishOTLPProfiles produces one tt.profiles record. body is req's protobuf encoding (nil = marshal), wire the bytes as
// received when they are its gzip'ed form.
func (s *Server) publishOTLPProfiles(req *colprofilespb.ExportProfilesServiceRequest, body, wire []byte, m meta) error {
	if len(req.GetResourceProfiles()) == 0 {
		return nil
	}
	var err error
	if body == nil {
		if body, err = proto.Marshal(req); err != nil {
			return &ingestError{status: http.StatusBadRequest, msg: "encode: " + err.Error()}
		}
	}
	value := body
	switch {
	case len(wire) < len(body) && profiling.IsGzip(wire):
		value = wire
	case len(body) > otlpProfileGzipAbove:
		if value, err = gzipBytes(body); err != nil {
			return &ingestError{status: http.StatusInternalServerError, msg: "compress: " + err.Error()}
		}
	}
	if len(value) > topics.MaxLargeRecordBytes {
		return &ingestError{status: http.StatusRequestEntityTooLarge, msg: "profiles request too large"}
	}
	return s.send(topics.Profiles, [][]byte{value}, m, kgo.RecordHeader{Key: topics.HeaderFormat, Value: []byte(profiling.FormatOTLP)})
}

type profilesSvc struct {
	colprofilespb.UnimplementedProfilesServiceServer
	s *Server
}

func (p profilesSvc) Export(ctx context.Context, req *colprofilespb.ExportProfilesServiceRequest) (*colprofilespb.ExportProfilesServiceResponse, error) {
	return &colprofilespb.ExportProfilesServiceResponse{}, p.s.grpcExport(ctx, sigProfiles, req)
}
