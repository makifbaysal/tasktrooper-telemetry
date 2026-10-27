package gw

import (
	"context"
	"errors"
	"net/http"

	"github.com/tasktrooper/telemetry/services/ingest/internal/auth"
)

// SetInternal enables the internal service credential (contracts §16). nil (TT_INTERNAL_TOKEN unset) keeps it off:
// requests carrying x-trooper-internal are then rejected with 401.
func (s *Server) SetInternal(in *auth.Internal) { s.internal = in }

var (
	errInternalInvalid = &ingestError{status: http.StatusUnauthorized, msg: "invalid internal credential", authReason: "internal"}
	errInternalBrowser = &ingestError{status: http.StatusForbidden, msg: "internal credential is not accepted from browsers", authReason: "internal_origin"}
)

// authenticate resolves the tenant for a request. When x-trooper-internal is present the ingest key is ignored and
// the internal token + x-trooper-project are checked instead; a browser Origin always disqualifies the internal path.
func (s *Server) authenticate(ctx context.Context, internalToken, project, origin, rawKey string) (*auth.Key, error) {
	if internalToken != "" {
		if origin != "" {
			return nil, errInternalBrowser
		}
		if !s.internal.Enabled() {
			return nil, errInternalInvalid
		}
		key, err := s.internal.Lookup(ctx, internalToken, project)
		if err != nil {
			if errors.Is(err, auth.ErrInvalidKey) {
				return nil, errInternalInvalid
			}
			s.log.Error("internal project lookup failed", "err", err)
			return nil, &ingestError{status: http.StatusServiceUnavailable, msg: "auth backend unavailable", retryAfter: 5}
		}
		return key, nil
	}
	key, err := s.keys.Lookup(ctx, rawKey)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidKey) {
			return nil, errUnauthorized
		}
		s.log.Error("key lookup failed", "err", err)
		return nil, &ingestError{status: http.StatusServiceUnavailable, msg: "auth backend unavailable", retryAfter: 5}
	}
	if origin != "" && !key.OriginAllowed(origin) {
		return nil, errForbidden
	}
	return key, nil
}
