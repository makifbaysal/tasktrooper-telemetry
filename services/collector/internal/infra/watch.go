package infra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Reflector keeps an in-memory copy of one Kubernetes resource kind: an initial paged `list`, then
// `watch?resourceVersion=<rv>&allowWatchBookmarks=true` from the list's resourceVersion. Bookmarks
// advance the resourceVersion; a 410 Gone (expired resourceVersion, as an HTTP status or an ERROR
// watch event) triggers a fresh list that replaces the store. Watch streams that end (server
// timeoutSeconds, network) are resumed from the last resourceVersion; failures back off
// exponentially (with jitter). Plain net/http + streaming JSON, no client-go (contracts §22).
type Reflector[T any] struct {
	C     *KubeClient
	Path  string     // "/api/v1/pods"
	Extra url.Values // e.g. fieldSelector=type=Warning (sent on list and watch)
	Meta  func(*T) *ObjectMeta
	// NoStore: do not keep objects (events only need OnList / OnEvent).
	NoStore bool
	// OnList is called after every successful (re)list with all items; OnEvent for every
	// ADDED / MODIFIED / DELETED watch event. Both run on the reflector's goroutine.
	OnList  func(ctx context.Context, items []T, relist bool)
	OnEvent func(ctx context.Context, typ string, obj *T)

	// Backoff bounds (defaults 1 s … 30 s; ForbiddenBackoff 5 min for 401/403/404 — a kind the
	// service account may not read or an API group the cluster does not serve).
	MinBackoff, MaxBackoff, ForbiddenBackoff time.Duration
	// WatchTimeout is the server-side timeoutSeconds of one watch request (default 5–10 min,
	// randomised so the kinds do not reconnect in lockstep).
	WatchTimeout time.Duration

	mu      sync.RWMutex
	items   map[string]T
	rv      string
	synced  bool
	lastErr error
	lists   int // successful lists (tests)
	ready   chan struct{}
	once    sync.Once
}

// ErrGone is a 410 Gone from list continue / watch: the resourceVersion is too old.
var ErrGone = errors.New("410 Gone: resourceVersion expired")

type watchEvent struct {
	Type   string          `json:"type"`
	Object json.RawMessage `json:"object"`
}

type statusObject struct {
	Kind    string `json:"kind"`
	Code    int    `json:"code"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func (r *Reflector[T]) init() {
	r.once.Do(func() {
		r.ready = make(chan struct{})
		if r.MinBackoff <= 0 {
			r.MinBackoff = time.Second
		}
		if r.MaxBackoff <= 0 {
			r.MaxBackoff = 30 * time.Second
		}
		if r.ForbiddenBackoff <= 0 {
			r.ForbiddenBackoff = 5 * time.Minute
		}
	})
}

// Ready is closed once the first list succeeded or failed (Synced then tells which).
func (r *Reflector[T]) Ready() <-chan struct{} {
	r.init()
	return r.ready
}

// Synced reports whether the store holds a complete list, and the last list/watch error.
func (r *Reflector[T]) Synced() (bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.synced, r.lastErr
}

// Items returns a snapshot of the stored objects.
func (r *Reflector[T]) Items() []T {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]T, 0, len(r.items))
	for _, v := range r.items {
		out = append(out, v)
	}
	return out
}

// ResourceVersion is the last seen resourceVersion (tests).
func (r *Reflector[T]) ResourceVersion() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.rv
}

// Lists is the number of successful lists (initial + relists).
func (r *Reflector[T]) Lists() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lists
}

func (r *Reflector[T]) setErr(err error) {
	r.mu.Lock()
	r.lastErr = err
	r.mu.Unlock()
	r.markReady()
}

func (r *Reflector[T]) markReady() {
	select {
	case <-r.ready:
	default:
		close(r.ready)
	}
}

func key(m *ObjectMeta) string {
	if m.Namespace == "" {
		return m.Name
	}
	return m.Namespace + "/" + m.Name
}

// Run lists and watches until ctx is done.
func (r *Reflector[T]) Run(ctx context.Context) {
	r.init()
	backoff := r.MinBackoff
	relist := false
	for ctx.Err() == nil {
		if err := r.list(ctx, relist); err != nil {
			if ctx.Err() != nil {
				return
			}
			r.setErr(err)
			wait := backoff
			if forbidden(err) {
				wait = r.ForbiddenBackoff
			}
			if !sleep(ctx, jitter(wait)) {
				return
			}
			backoff = min(backoff*2, r.MaxBackoff)
			continue
		}
		relist = true
		backoff = r.MinBackoff
		// watch from the list's resourceVersion until it expires
		failures := 0
		for ctx.Err() == nil {
			err := r.watch(ctx)
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, ErrGone) {
				break // relist
			}
			if err == nil {
				failures = 0 // server closed a healthy stream (timeoutSeconds): resume at once
				continue
			}
			r.setErr(err)
			failures++
			if forbidden(err) || failures >= 5 {
				break // something is off: relist (with the list's own backoff)
			}
			if !sleep(ctx, jitter(min(r.MinBackoff<<failures, r.MaxBackoff))) {
				return
			}
		}
	}
}

func forbidden(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && (he.Status == http.StatusUnauthorized || he.Status == http.StatusForbidden || he.Status == http.StatusNotFound)
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// list pages through the collection and replaces the store.
func (r *Reflector[T]) list(ctx context.Context, relist bool) error {
	var all []T
	cont, rv := "", ""
	for {
		q := url.Values{}
		for k, v := range r.Extra {
			q[k] = v
		}
		q.Set("limit", "500")
		if cont != "" {
			q.Set("continue", cont)
		}
		var page struct {
			Metadata struct {
				Continue        string `json:"continue"`
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
			Items []T `json:"items"`
		}
		if err := r.C.Get(ctx, r.Path+"?"+q.Encode(), &page); err != nil {
			var he *HTTPError
			if errors.As(err, &he) && he.Status == http.StatusGone {
				cont = "" // continue token expired mid-list: start over
				all = all[:0]
				continue
			}
			return fmt.Errorf("list %s: %w", r.Path, err)
		}
		all = append(all, page.Items...)
		rv = page.Metadata.ResourceVersion
		if page.Metadata.Continue == "" {
			break
		}
		cont = page.Metadata.Continue
	}
	if !r.NoStore {
		m := make(map[string]T, len(all))
		for i := range all {
			m[key(r.Meta(&all[i]))] = all[i]
		}
		r.mu.Lock()
		r.items = m
		r.mu.Unlock()
	}
	r.mu.Lock()
	r.rv, r.synced, r.lastErr = rv, true, nil
	r.lists++
	r.mu.Unlock()
	if r.OnList != nil {
		r.OnList(ctx, all, relist)
	}
	r.markReady() // after OnList: a waiting Scrape sees the first list fully handled
	return nil
}

// watch streams changes from the current resourceVersion. nil = the server ended the stream
// normally; ErrGone = relist.
func (r *Reflector[T]) watch(ctx context.Context) error {
	timeout := r.WatchTimeout
	if timeout <= 0 {
		timeout = 5*time.Minute + time.Duration(rand.Int64N(int64(5*time.Minute)))
	}
	q := url.Values{}
	for k, v := range r.Extra {
		q[k] = v
	}
	q.Set("watch", "1")
	q.Set("allowWatchBookmarks", "true")
	q.Set("timeoutSeconds", strconv.Itoa(max(1, int(timeout/time.Second))))
	if rv := r.ResourceVersion(); rv != "" {
		q.Set("resourceVersion", rv)
	}
	// The server closes the stream after timeoutSeconds; a silent, half-open connection is
	// cut a bit later by this deadline.
	wctx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()
	body, err := r.C.OpenStream(wctx, r.Path+"?"+q.Encode())
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && he.Status == http.StatusGone {
			return ErrGone
		}
		return fmt.Errorf("watch %s: %w", r.Path, err)
	}
	defer body.Close()
	dec := json.NewDecoder(body)
	for {
		var ev watchEvent
		if err := dec.Decode(&ev); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if wctx.Err() != nil && ctx.Err() == nil {
				return nil // our own deadline: reconnect
			}
			return fmt.Errorf("watch %s: decode: %w", r.Path, err)
		}
		switch ev.Type {
		case "ADDED", "MODIFIED", "DELETED":
			var obj T
			if err := json.Unmarshal(ev.Object, &obj); err != nil {
				return fmt.Errorf("watch %s: decode object: %w", r.Path, err)
			}
			m := r.Meta(&obj)
			r.mu.Lock()
			if !r.NoStore {
				if r.items == nil {
					r.items = map[string]T{}
				}
				if ev.Type == "DELETED" {
					delete(r.items, key(m))
				} else {
					r.items[key(m)] = obj
				}
			}
			if m.ResourceVersion != "" {
				r.rv = m.ResourceVersion
			}
			r.lastErr = nil
			r.mu.Unlock()
			if r.OnEvent != nil {
				r.OnEvent(ctx, ev.Type, &obj)
			}
		case "BOOKMARK":
			var b struct {
				Metadata ObjectMeta `json:"metadata"`
			}
			if json.Unmarshal(ev.Object, &b) == nil && b.Metadata.ResourceVersion != "" {
				r.mu.Lock()
				r.rv = b.Metadata.ResourceVersion
				r.mu.Unlock()
			}
		case "ERROR":
			var st statusObject
			_ = json.Unmarshal(ev.Object, &st)
			if st.Code == http.StatusGone || st.Reason == "Expired" || st.Reason == "Gone" {
				return ErrGone
			}
			return fmt.Errorf("watch %s: error event %d %s: %s", r.Path, st.Code, st.Reason, st.Message)
		default:
			return fmt.Errorf("watch %s: unexpected event type %q", r.Path, ev.Type)
		}
	}
}
