package infra

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"testing"
	"time"
)

func podLine(typ, name, rv string) string {
	return `{"type":"` + typ + `","object":{"metadata":{"name":"` + name + `","namespace":"ns","uid":"u-` + name + `","resourceVersion":"` + rv + `"},"status":{"phase":"Running"}}}`
}

func podNames(r *Reflector[Pod]) []string {
	var out []string
	for _, p := range r.Items() {
		out = append(out, p.Metadata.Name)
	}
	sort.Strings(out)
	return out
}

// TestReflectorWatch: list → watch from the list's resourceVersion; ADDED / MODIFIED / DELETED
// update the store; a BOOKMARK advances the resourceVersion used on reconnect; a stream closed by
// the server is resumed (no relist); an ERROR 410 event and an HTTP 410 relist; a 500 on watch
// backs off and reconnects; the relist replaces the store (objects deleted meanwhile disappear).
func TestReflectorWatch(t *testing.T) {
	f := &fakeAPI{objects: map[string]string{}, rv: "10"}
	f.set("/api/v1/pods", `[{"metadata":{"name":"a","namespace":"ns","uid":"u-a"}},{"metadata":{"name":"b","namespace":"ns","uid":"u-b"}}]`)
	srv := httptest.NewServer(f)
	defer srv.Close()
	kc, _ := KubeEnv{APIURL: srv.URL, Token: "test-token"}.API()
	var events []string
	evCh := make(chan string, 64)
	r := &Reflector[Pod]{C: kc, Path: "/api/v1/pods", Meta: func(p *Pod) *ObjectMeta { return &p.Metadata },
		MinBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond,
		OnEvent: func(_ context.Context, typ string, p *Pod) { evCh <- typ + " " + p.Metadata.Name }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	<-r.Ready()
	if ok, err := r.Synced(); !ok || err != nil {
		t.Fatalf("synced=%v err=%v", ok, err)
	}
	if got := podNames(r); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("after list: %v", got)
	}
	waitFor(t, func() bool { return len(f.watchRV("/api/v1/pods")) == 1 })
	if rv := f.watchRV("/api/v1/pods")[0]; rv != "10" {
		t.Fatalf("watch from rv %q, want the list's 10", rv)
	}

	next := func() string {
		select {
		case e := <-evCh:
			events = append(events, e)
			return e
		case <-time.After(5 * time.Second):
			t.Fatal("no watch event")
			return ""
		}
	}
	f.push("/api/v1/pods", podLine("ADDED", "c", "11"))
	f.push("/api/v1/pods", podLine("MODIFIED", "a", "12"))
	f.push("/api/v1/pods", podLine("DELETED", "b", "13"))
	for _, want := range []string{"ADDED c", "MODIFIED a", "DELETED b"} {
		if got := next(); got != want {
			t.Fatalf("event %q, want %q", got, want)
		}
	}
	if got := podNames(r); !slices.Equal(got, []string{"a", "c"}) {
		t.Fatalf("after watch events: %v", got)
	}

	// bookmark, then the server closes the stream: resume from the bookmark's rv, no relist
	f.push("/api/v1/pods", `{"type":"BOOKMARK","object":{"kind":"Pod","metadata":{"resourceVersion":"50"}}}`)
	waitFor(t, func() bool { return r.ResourceVersion() == "50" })
	f.push("/api/v1/pods", "CLOSE")
	waitFor(t, func() bool { return len(f.watchRV("/api/v1/pods")) == 2 })
	if rv := f.watchRV("/api/v1/pods")[1]; rv != "50" {
		t.Fatalf("resumed watch from rv %q, want bookmark 50", rv)
	}
	if n := f.listCount("/api/v1/pods"); n != 1 {
		_, e := r.Synced()
		t.Fatalf("lists = %d after a normal stream end, want 1 (no relist) %v %v", n, e, f.watchRV("/api/v1/pods"))
	}

	// ERROR 410 event → relist; the relist replaces the store
	f.rvSet("60")
	f.set("/api/v1/pods", `[{"metadata":{"name":"a","namespace":"ns","uid":"u-a"}},{"metadata":{"name":"d","namespace":"ns","uid":"u-d"}}]`)
	f.push("/api/v1/pods", `{"type":"ERROR","object":{"kind":"Status","status":"Failure","reason":"Expired","code":410,"message":"too old resource version: 50 (55)"}}`)
	waitFor(t, func() bool { return f.listCount("/api/v1/pods") == 2 && len(f.watchRV("/api/v1/pods")) == 3 })
	if got := podNames(r); !slices.Equal(got, []string{"a", "d"}) {
		t.Fatalf("after relist: %v (c must be gone)", got)
	}
	if rv := f.watchRV("/api/v1/pods")[2]; rv != "60" {
		t.Fatalf("watch after relist from rv %q, want 60", rv)
	}

	// HTTP 410 on the watch request → relist
	f.setWatchStatus("/api/v1/pods", http.StatusGone)
	f.push("/api/v1/pods", "CLOSE")
	waitFor(t, func() bool { return f.listCount("/api/v1/pods") == 3 })

	// 500 on watch → backoff + reconnect from the same rv, no relist
	waitFor(t, func() bool { return len(f.watchRV("/api/v1/pods")) == 5 })
	f.setWatchStatus("/api/v1/pods", http.StatusInternalServerError)
	f.push("/api/v1/pods", "CLOSE")
	waitFor(t, func() bool { return len(f.watchRV("/api/v1/pods")) == 7 })
	rvs := f.watchRV("/api/v1/pods")
	if rvs[5] != "60" || rvs[6] != "60" {
		t.Fatalf("reconnect rvs = %v", rvs)
	}
	if n := f.listCount("/api/v1/pods"); n != 3 {
		t.Fatalf("lists = %d after a 500, want 3", n)
	}
	if ok, _ := r.Synced(); !ok {
		t.Fatal("store must stay synced across watch errors")
	}
}

// TestReflectorForbidden: a kind the service account may not list never syncs and reports the
// error; nodes/pods failing makes the cluster scrape fail, optional kinds do not.
func TestReflectorForbidden(t *testing.T) {
	f := &fakeAPI{objects: map[string]string{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	kc, _ := KubeEnv{APIURL: srv.URL, Token: "test-token"}.API()
	r := &Reflector[cronJob]{C: kc, Path: "/apis/batch/v1/cronjobs", Meta: func(c *cronJob) *ObjectMeta { return &c.Metadata }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	<-r.Ready()
	ok, err := r.Synced()
	if ok || !forbidden(err) {
		t.Fatalf("synced=%v err=%v, want forbidden", ok, err)
	}
	if len(r.Items()) != 0 {
		t.Fatal("items from a failed list")
	}
}

func (f *fakeAPI) rvSet(rv string) {
	f.mu.Lock()
	f.rv = rv
	f.mu.Unlock()
}

func (f *fakeAPI) setWatchStatus(path string, st int) {
	f.mu.Lock()
	if f.watchStatus == nil {
		f.watchStatus = map[string]int{}
	}
	f.watchStatus[path] = st
	f.mu.Unlock()
}
