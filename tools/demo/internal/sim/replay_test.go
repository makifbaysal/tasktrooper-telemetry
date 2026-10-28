package sim

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"
)

func appNamed(t *testing.T, name string) clientApp {
	t.Helper()
	for _, a := range apps() {
		if a.name == name {
			return a
		}
	}
	t.Fatalf("no app %s", name)
	return clientApp{}
}

// testRecorder returns a recorder without its flush loop.
func testRecorder(t *testing.T, app string, dev int) *recorder {
	a := appNamed(t, app)
	s := &session{app: a, dev: a.devices[dev], id: "s_" + randHex(16), anon: "a_" + randHex(16)}
	tr := &Traffic{Replay: true, ReplayRate: 1}
	s.t = tr
	r := &recorder{t: tr, s: s, web: !a.mobile, touch: a.mobile || s.dev.typ == "mobile", W: float64(s.dev.w), H: float64(s.dev.h),
		density: 3, typed: map[int]int{}, mode: modeRecording, lastFill: nowMs(), wake: make(chan struct{}, 1), done: make(chan struct{})}
	s.rec = r
	return r
}

// drive simulates a session of n views with actions and an error on the second view.
func drive(r *recorder, n int) {
	clock := nowMs()
	r.clock = func() int64 { return clock }
	for i := 0; i < n; i++ {
		p := r.s.app.pages[i%len(r.s.app.pages)]
		vid := "v_" + randHex(12)
		clock += 20000
		ts := clock
		r.viewBegin(vid, p, i == 0)
		r.onRUM("VIEW_START", ts+300, nil)
		r.onRUM("ACTION", ts+1500, map[string]any{"name": "Search", "rage": false})
		r.onRUM("ACTION", ts+3500, map[string]any{"name": "Open", "rage": i == 1})
		if i == 1 {
			r.onRUM("ERROR", ts+4000, nil)
		}
		r.mu.Lock()
		r.fill(ts + 9000)
		r.mu.Unlock()
	}
}

func allSegments(r *recorder) []segment {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []segment
	for {
		sg := r.cut(nowMs() + int64(time.Hour/time.Millisecond))
		if sg == nil {
			return out
		}
		out = append(out, *sg)
	}
}

func TestRRWebStreamIsConsistent(t *testing.T) {
	for _, app := range []string{"shop-web", "backoffice-web", "seller-portal", "landing-web"} {
		t.Run(app, func(t *testing.T) {
			r := testRecorder(t, app, 0)
			drive(r, 6)
			segs := allSegments(r)
			if len(segs) != 1 {
				t.Fatalf("segments %d", len(segs))
			}
			raw, err := json.Marshal(r.payload(0, segs[0]))
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Format string `json:"format"`
				Full   bool   `json:"has_full_snapshot"`
				Stats  struct {
					Clicks, Errors int
					Rage           int `json:"rage_clicks"`
				} `json:"stats"`
				Events []struct {
					Type      int             `json:"type"`
					Timestamp int64           `json:"timestamp"`
					Data      json.RawMessage `json:"data"`
				} `json:"events"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			if body.Format != "rrweb" || !body.Full || body.Events[0].Type != rrMeta || body.Events[1].Type != rrFull {
				t.Fatalf("stream must start with Meta + FullSnapshot: format=%s full=%v first=%d", body.Format, body.Full, body.Events[0].Type)
			}
			if body.Stats.Clicks == 0 || body.Stats.Errors != 1 {
				t.Fatalf("stats %+v", body.Stats)
			}
			mirror := map[int]bool{}
			var walk func(n map[string]any)
			walk = func(n map[string]any) {
				id := int(n["id"].(float64))
				if mirror[id] {
					t.Fatalf("duplicate node id %d", id)
				}
				mirror[id] = true
				if kids, ok := n["childNodes"].([]any); ok {
					for _, k := range kids {
						walk(k.(map[string]any))
					}
				}
			}
			last := int64(0)
			for _, e := range body.Events {
				if e.Timestamp < last {
					t.Fatalf("events not sorted")
				}
				last = e.Timestamp
				var d map[string]any
				_ = json.Unmarshal(e.Data, &d)
				switch e.Type {
				case rrFull:
					mirror = map[int]bool{}
					walk(d["node"].(map[string]any))
				case rrIncr:
					switch int(d["source"].(float64)) {
					case srcMutation:
						for _, x := range d["removes"].([]any) {
							rm := x.(map[string]any)
							if !mirror[int(rm["id"].(float64))] {
								t.Fatalf("remove of unknown node")
							}
							delete(mirror, int(rm["id"].(float64)))
						}
						for _, x := range d["adds"].([]any) {
							ad := x.(map[string]any)
							if !mirror[int(ad["parentId"].(float64))] {
								t.Fatalf("add under unknown parent %v", ad["parentId"])
							}
							id := int(ad["node"].(map[string]any)["id"].(float64))
							if mirror[id] {
								t.Fatalf("add of existing id %d", id)
							}
							mirror[id] = true
						}
					case srcInteraction, srcInput:
						if id := int(d["id"].(float64)); !mirror[id] {
							t.Fatalf("interaction on unknown node %d", id)
						}
					}
				}
			}
		})
	}
}

func TestWireframeFrames(t *testing.T) {
	for _, app := range []string{"shop-mobile", "courier-app"} {
		r := testRecorder(t, app, 1)
		drive(r, 5)
		segs := allSegments(r)
		p := r.payload(0, segs[0])
		if p["format"] != "wireframe" || p["resource"].(map[string]any)["platform"] != "android" {
			t.Fatalf("%s: %v %v", app, p["format"], p["resource"])
		}
		lastFrame, frames, touches := int64(0), 0, 0
		for _, e := range segs[0].events {
			m := e.(map[string]any)
			switch m["type"] {
			case "frame":
				ts := m["ts"].(int64)
				if lastFrame > 0 && ts-lastFrame < minFrameGap {
					t.Fatalf("%s: frames %d ms apart", app, ts-lastFrame)
				}
				lastFrame = ts
				frames++
				if len(m["nodes"].([]any)) < 5 {
					t.Fatalf("%s: frame with %d nodes", app, len(m["nodes"].([]any)))
				}
			case "touch":
				touches++
			}
		}
		if frames < 10 || touches == 0 {
			t.Fatalf("%s: frames=%d touches=%d", app, frames, touches)
		}
	}
}

func TestScreenTitles(t *testing.T) {
	for in, want := range map[string]string{"/": "Home", "/account/orders/:id": "Orders", "/p/:slug": "P", "OrderTracking": "OrderTracking", "/products/:id/edit": "Edit"} {
		if got := screenTitle(in); got != want {
			t.Errorf("screenTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBufferedSessionFlushesOnError: an unsampled session sends nothing until its first error, then its
// buffer (starting with a full snapshot) and the rest.
func TestBufferedSessionFlushesOnError(t *testing.T) {
	var mu sync.Mutex
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		b, _ := io.ReadAll(zr)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	}))
	defer srv.Close()
	a := appNamed(t, "shop-web")
	tr := &Traffic{Replay: true, ReplayRate: 0, IngestURL: srv.URL, IngestKey: "k", rum: srv.Client(), Log: slog.Default()}
	s := &session{t: tr, app: a, dev: a.devices[0], id: "s_" + randHex(16), anon: "a_x"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.rec = tr.newRecorder(ctx, s)
	s.rec.mode = modeBuffering
	s.rec.viewBegin("v1", a.pages[0], true)
	s.rec.onRUM("VIEW_START", nowMs(), nil)
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	if len(got) != 0 {
		t.Fatalf("buffered session sent %d segments before an error", len(got))
	}
	mu.Unlock()
	s.rec.onRUM("ERROR", nowMs(), nil)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.rec.close()
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("nothing flushed after the error")
	}
	first := got[0]
	if first["segment"].(float64) != 0 || first["has_full_snapshot"] != true || first["stats"].(map[string]any)["errors"].(float64) != 1 {
		t.Fatalf("first flushed segment: segment=%v full=%v stats=%v", first["segment"], first["has_full_snapshot"], first["stats"])
	}
	if !regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`).MatchString(first["session_id"].(string)) {
		t.Fatalf("session id %q", first["session_id"])
	}
}
