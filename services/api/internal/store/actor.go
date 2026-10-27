package store

import "context"

// Actor is who performs a change (for incident timelines and acked_by): a user, via the api or a chat button.
type Actor struct {
	UserID int64
	Email  string
	Via    string // api | slack
}

type actorKey struct{}

// WithActor returns ctx carrying a.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorOf returns the actor of ctx (zero value: anonymous api caller).
func ActorOf(ctx context.Context) Actor {
	a, _ := ctx.Value(actorKey{}).(Actor)
	return a
}

func (a Actor) via() string {
	if a.Via == "" {
		return "api"
	}
	return a.Via
}
