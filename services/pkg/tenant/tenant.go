// Package tenant carries the (org, project) a request or record belongs to.
package tenant

import (
	"context"
	"errors"
	"strconv"
)

// Tenant identifies the owner of every row; every query filters by both ids.
type Tenant struct {
	OrgID     uint32
	ProjectID uint32
}

type ctxKey struct{}

// With returns ctx carrying t.
func With(ctx context.Context, t Tenant) context.Context { return context.WithValue(ctx, ctxKey{}, t) }

// From returns the tenant in ctx.
func From(ctx context.Context) (Tenant, bool) {
	t, ok := ctx.Value(ctxKey{}).(Tenant)
	return t, ok
}

// ErrNoTenant is returned when a context has no tenant.
var ErrNoTenant = errors.New("tenant: missing in context")

// Must returns the tenant in ctx or ErrNoTenant.
func Must(ctx context.Context) (Tenant, error) {
	t, ok := From(ctx)
	if !ok {
		return Tenant{}, ErrNoTenant
	}
	return t, nil
}

// Headers encodes t as Kafka header values.
func (t Tenant) Headers() (org, project string) {
	return strconv.FormatUint(uint64(t.OrgID), 10), strconv.FormatUint(uint64(t.ProjectID), 10)
}

// Parse decodes header values produced by Headers.
func Parse(org, project string) (Tenant, error) {
	o, err := strconv.ParseUint(org, 10, 32)
	if err != nil {
		return Tenant{}, err
	}
	p, err := strconv.ParseUint(project, 10, 32)
	if err != nil {
		return Tenant{}, err
	}
	return Tenant{OrgID: uint32(o), ProjectID: uint32(p)}, nil
}
