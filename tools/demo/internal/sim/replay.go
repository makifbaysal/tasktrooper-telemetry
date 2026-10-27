package sim

// Session replay for the simulated client apps (contracts §23, ADR-021): web apps record rrweb 2.x
// events (Meta + FullSnapshot, DOM mutations on route changes, mouse moves, clicks, scrolls, masked
// inputs), mobile apps record wireframes (screen, full frames, touches, keyboard). Events are produced
// at the same moments as the session's RUM events (same session_id, same clock), so the player's
// timeline (rum_events) lines up with the recording. Segments are cut every ≤ 5 s and POSTed gzip'ed
// to ingest-gw /v1/replay.
//
// Sampling like the SDKs: a session is recorded with probability ReplayRate; the others keep the last
// 30–60 s in memory (with a checkout snapshot every 30 s) and are flushed + recorded from the first
// ERROR on (onErrorSampleRate 1.0).

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tasktrooper/telemetry/tools/demo/internal/world"
)

const (
	segmentEvery   = 5 * time.Second
	checkoutEvery  = 30 * time.Second // buffer mode: fresh full snapshot so the buffer stays playable
	minFrameGap    = 500              // ms between wireframe frames (≤ 2 frames/s)
	replaySDK      = "@trooper/replay"
	replaySDKVer   = "0.1.0"
	modeRecording  = 1
	modeBuffering  = 2
	webCSS         = `html,body{margin:0;padding:0;background:#fff;font:14px/1.3 -apple-system,"Segoe UI",Roboto,Helvetica,Arial,sans-serif}#app{position:relative}header{position:absolute;top:0;left:0;right:0;z-index:1}main{position:relative}.b{position:absolute;box-sizing:border-box;overflow:hidden;display:flex;align-items:center;white-space:nowrap}.k-button{justify-content:center;border:0;font-weight:600}.k-input{border:1px solid #d9d9d9;padding:0 12px}`
	rrMeta         = 4
	rrFull         = 2
	rrIncr         = 3
	srcMutation    = 0
	srcMouseMove   = 1
	srcInteraction = 2
	srcScroll      = 3
	srcInput       = 5
	miMouseUp      = 0
	miMouseDown    = 1
	miClick        = 2
	miFocus        = 5
	miBlur         = 6
	miTouchStart   = 7
	miTouchEnd     = 9
)

// ---------------------------------------------------------------- rrweb DOM

type dnode struct {
	id      int
	typ     int // 0 document, 1 doctype, 2 element, 3 text
	tag     string
	attrs   map[string]any
	text    string
	isStyle bool
	kids    []*dnode
}

func (n *dnode) ser(withKids bool) map[string]any {
	m := map[string]any{"type": n.typ, "id": n.id}
	switch n.typ {
	case 0:
		m["compatMode"] = "CSS1Compat"
	case 1:
		m["name"], m["publicId"], m["systemId"] = "html", "", ""
	case 2:
		m["tagName"] = n.tag
		attrs := n.attrs
		if attrs == nil {
			attrs = map[string]any{}
		}
		m["attributes"] = attrs
	case 3:
		m["textContent"] = n.text
		if n.isStyle {
			m["isStyle"] = true
		}
	}
	if n.typ == 0 || n.typ == 2 {
		kids := []any{}
		if withKids {
			for _, k := range n.kids {
				kids = append(kids, k.ser(true))
			}
		}
		m["childNodes"] = kids
	}
	return m
}

// ---------------------------------------------------------------- events

type rrEvent struct {
	Type      int   `json:"type"`
	Data      any   `json:"data"`
	Timestamp int64 `json:"timestamp"`
}

type tsEvent struct {
	ts               int64
	ev               any
	full             bool // FullSnapshot / wireframe frame
	clicks, inputs   int
	rage, errs       int
	viewID, viewName string
}

type target struct {
	b  box
	id int // rrweb node id (web) / wireframe node id (mobile)
}

type segment struct {
	start, end       int64
	events           []any
	full             bool
	clicks, inputs   int
	rage, errs       int
	viewID, viewName string
}

// recorder records one session.
type recorder struct {
	t       *Traffic
	s       *session
	web     bool
	touch   bool // touch device (mobile web or app): no mouse moves
	W, H    float64
	density float64

	mu       sync.Mutex
	mode     int
	evs      []tsEvent
	buf      []segment // buffer mode: cut, not sent
	segNo    int
	lastFill int64
	lastCut  int64
	lastCkpt int64
	prevCkpt int64
	errFlush bool
	stopped  bool

	// current screen
	viewID, viewName string
	page             page
	chrome, content  []box
	targets          []target
	chromeTargets    []target
	docH, scrollY    float64
	mx, my           float64
	typed            map[int]int // box index (content) → typed characters
	lastFrame        int64

	// web DOM
	nextID int
	doc    *dnode
	main   *dnode

	clock func() int64 // tests
	wake  chan struct{}
	done  chan struct{}
}

func (t *Traffic) newRecorder(ctx context.Context, s *session) *recorder {
	if !t.Replay {
		return nil
	}
	r := &recorder{t: t, s: s, web: !s.app.mobile, touch: s.app.mobile || s.dev.typ == "mobile",
		W: float64(s.dev.w), H: float64(s.dev.h), density: 3, typed: map[int]int{},
		mode: modeBuffering, wake: make(chan struct{}, 1), done: make(chan struct{})}
	if s.dev.osName == "Android" {
		r.density = 2.625
	}
	if rand.Float64() < t.ReplayRate {
		r.mode = modeRecording
	}
	r.mx, r.my = r.W/2, r.H/2
	r.lastFill = time.Now().UnixMilli()
	go r.loop(ctx)
	return r
}

func nowMs() int64 { return time.Now().UnixMilli() }

func (r *recorder) now() int64 {
	if r.clock != nil {
		return r.clock()
	}
	return nowMs()
}

func (r *recorder) push(e tsEvent) {
	if e.ts <= r.lastCut {
		e.ts = r.lastCut + 1 // never before an already sent segment
	}
	if e.viewID == "" {
		e.viewID, e.viewName = r.viewID, r.viewName
	}
	r.evs = append(r.evs, e)
}

// dropFuture removes scheduled (not yet happened) interactions, e.g. on navigation.
func (r *recorder) dropFuture(now int64) {
	kept := r.evs[:0]
	for _, e := range r.evs {
		if e.ts <= now {
			kept = append(kept, e)
		}
	}
	r.evs = kept
	r.lastFrame = min(r.lastFrame, now)
}

// ---------------------------------------------------------------- hooks (called by the session)

// viewBegin: navigation to p; the view shows skeletons until its data arrives (viewLoaded).
func (r *recorder) viewBegin(viewID string, p page, initial bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.dropFuture(now)
	r.lastFill = max(r.lastFill, now) // no idle activity before the screen exists
	r.viewID, r.viewName, r.page = viewID, p.name, p
	r.typed = map[int]int{}
	r.chrome, r.content, r.docH = screenLayout(r.s.app, p, r.W, r.H, true)
	if r.web {
		if initial || r.doc == nil {
			r.webCheckout(now)
		} else {
			r.webSwap(now)
			r.scrollTo(now, 0)
		}
	} else {
		r.scrollY = 0
		r.push(tsEvent{ts: now, ev: map[string]any{"type": "screen", "ts": now, "width": r.W, "height": r.H, "density": r.density, "name": screenTitle(p.name) + "Screen"}})
		r.frame(now)
	}
	if r.lastCkpt == 0 {
		r.lastCkpt = now
	}
}

// onRUM mirrors a RUM event into the recording.
func (r *recorder) onRUM(typ string, ts int64, payload map[string]any) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch typ {
	case "VIEW_START": // data arrived: render the content
		r.chrome, r.content, r.docH = screenLayout(r.s.app, r.page, r.W, r.H, false)
		if r.web {
			r.webSwap(ts)
		} else {
			r.frame(ts)
		}
		r.scheduleForm(ts)
	case "ACTION":
		name, _ := payload["name"].(string)
		rage, _ := payload["rage"].(bool)
		r.interact(ts, name, rage)
	case "ERROR":
		r.push(tsEvent{ts: ts, errs: 1, ev: nil})
		if r.mode == modeBuffering {
			r.mode, r.errFlush = modeRecording, true
			select {
			case r.wake <- struct{}{}:
			default:
			}
		}
	}
}

// close ends the recording: the last segment is sent (recording) or the buffer dropped.
func (r *recorder) close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if !r.stopped {
		r.stopped = true
		close(r.done)
	}
	r.mu.Unlock()
}

// ---------------------------------------------------------------- web (rrweb)

func (r *recorder) id() int { r.nextID++; return r.nextID }

func (r *recorder) el(tag string, attrs map[string]any, kids ...*dnode) *dnode {
	return &dnode{id: r.id(), typ: 2, tag: tag, attrs: attrs, kids: kids}
}

func (r *recorder) textNode(s string) *dnode { return &dnode{id: r.id(), typ: 3, text: s} }

func styleOf(b box) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "left:%.0fpx;top:%.0fpx;width:%.0fpx;height:%.0fpx;", b.x, b.y, b.w, b.h)
	if b.bg != "" {
		sb.WriteString("background:" + b.bg + ";")
	}
	if b.fg != "" {
		sb.WriteString("color:" + b.fg + ";")
	}
	if b.border != "" {
		sb.WriteString("border:1px solid " + b.border + ";")
	}
	if b.radius > 0 {
		fmt.Fprintf(&sb, "border-radius:%.0fpx;", b.radius)
	}
	if b.kind == "text" && b.h >= 28 {
		fmt.Fprintf(&sb, "font-size:%.0fpx;font-weight:600;", b.h*0.7)
	}
	return sb.String()
}

// boxNode renders a box (and its text) as DOM nodes.
func (r *recorder) boxNode(b box) *dnode {
	attrs := map[string]any{"class": "b k-" + b.kind, "style": styleOf(b)}
	tag := "div"
	switch b.kind {
	case "button":
		tag = "button"
	case "input":
		tag = "input"
		attrs["type"] = "text"
		if b.role == "password" {
			attrs["type"] = "password"
		}
		if b.role == "search" {
			attrs["type"], attrs["placeholder"] = "search", "Search"
		}
		attrs["value"] = ""
	}
	if b.unmask {
		attrs["data-trooper-unmask"] = ""
	}
	n := r.el(tag, attrs)
	if b.text != "" && tag != "input" {
		s := b.text
		if !b.unmask {
			s = maskText(s, '*')
		}
		n.kids = append(n.kids, r.textNode(s))
	}
	return n
}

func (r *recorder) contentNodes() []*dnode {
	r.targets = r.targets[:0]
	var out []*dnode
	for _, b := range r.content {
		n := r.boxNode(b)
		out = append(out, n)
		if b.role != "" {
			r.targets = append(r.targets, target{b: b, id: n.id})
		}
	}
	r.targets = append(r.targets, r.chromeTargets...)
	return out
}

// webCheckout emits Meta + FullSnapshot of the current screen (page load or buffer checkout).
func (r *recorder) webCheckout(ts int64) {
	r.nextID = 0
	doc := &dnode{id: r.id(), typ: 0}
	doc.kids = append(doc.kids, &dnode{id: r.id(), typ: 1})
	head := r.el("head", nil,
		r.el("meta", map[string]any{"charset": "utf-8"}),
		r.el("title", nil, r.textNode(r.s.app.name)),
		r.el("style", map[string]any{"_cssText": webCSS}, &dnode{id: r.id(), typ: 3, text: webCSS, isStyle: true}))
	header := r.el("header", map[string]any{"style": fmt.Sprintf("height:%.0fpx", r.headerH())})
	r.chromeTargets = nil
	for _, b := range r.chrome {
		n := r.boxNode(b)
		header.kids = append(header.kids, n)
		if b.role != "" {
			r.chromeTargets = append(r.chromeTargets, target{b: b, id: n.id})
		}
	}
	r.main = r.el("main", map[string]any{"style": fmt.Sprintf("height:%.0fpx", r.docH)})
	// content boxes are positioned relative to the document: main starts below the header
	r.main.kids = r.contentNodes()
	body := r.el("body", nil, r.el("div", map[string]any{"id": "app"}, header, r.main))
	doc.kids = append(doc.kids, r.el("html", map[string]any{"lang": "tr"}, head, body))
	r.doc = doc
	r.push(tsEvent{ts: ts, ev: rrEvent{Type: rrMeta, Timestamp: ts, Data: map[string]any{"href": viewURL(r.s.app, r.viewName), "width": r.W, "height": r.H}}})
	r.push(tsEvent{ts: ts, full: true, ev: rrEvent{Type: rrFull, Timestamp: ts, Data: map[string]any{
		"node": doc.ser(true), "initialOffset": map[string]any{"left": 0, "top": r.scrollY}}}})
	r.lastCkpt = ts
}

func (r *recorder) headerH() float64 {
	if len(r.chrome) > 0 {
		return r.chrome[0].h
	}
	return 0
}

// webSwap replaces the content of <main> (SPA route change / data loaded) with one mutation event.
func (r *recorder) webSwap(ts int64) {
	if r.main == nil {
		r.webCheckout(ts)
		return
	}
	removes := []any{}
	for _, k := range r.main.kids {
		removes = append(removes, map[string]any{"parentId": r.main.id, "id": k.id})
	}
	r.main.kids = r.contentNodes()
	adds := []any{}
	for _, n := range r.main.kids {
		adds = append(adds, map[string]any{"parentId": r.main.id, "nextId": nil, "node": n.ser(false)})
		for _, k := range n.kids {
			adds = append(adds, map[string]any{"parentId": n.id, "nextId": nil, "node": k.ser(false)})
		}
	}
	h := fmt.Sprintf("height:%.0fpx", r.docH)
	r.main.attrs = map[string]any{"style": h}
	r.push(tsEvent{ts: ts, ev: rrEvent{Type: rrIncr, Timestamp: ts, Data: map[string]any{"source": srcMutation,
		"texts": []any{}, "attributes": []any{map[string]any{"id": r.main.id, "attributes": map[string]any{"style": h}}},
		"removes": removes, "adds": adds}}})
}

func (r *recorder) scrollTo(ts int64, y float64) {
	y = math.Max(0, math.Min(y, r.docH-r.H))
	if y == r.scrollY {
		return
	}
	r.scrollY = y
	if r.web {
		docID := 1
		r.push(tsEvent{ts: ts, ev: rrEvent{Type: rrIncr, Timestamp: ts, Data: map[string]any{"source": srcScroll, "id": docID, "x": 0, "y": math.Round(y)}}})
		return
	}
	r.frame(ts)
}

// moveTo emits a mouse path to (x, y) (viewport coordinates) ending at ts.
func (r *recorder) moveTo(ts int64, x, y float64) {
	if r.touch || !r.web {
		r.mx, r.my = x, y
		return
	}
	n := 3 + rand.IntN(4)
	pos := make([]any, 0, n)
	for i := 1; i <= n; i++ {
		f := float64(i) / float64(n)
		pos = append(pos, map[string]any{"x": math.Round(r.mx + (x-r.mx)*f + rand.NormFloat64()*3), "y": math.Round(r.my + (y-r.my)*f + rand.NormFloat64()*3),
			"id": r.bodyID(), "timeOffset": -int64(float64(n-i) * 60)})
	}
	r.mx, r.my = x, y
	r.push(tsEvent{ts: ts, ev: rrEvent{Type: rrIncr, Timestamp: ts, Data: map[string]any{"source": srcMouseMove, "positions": pos}}})
}

func (r *recorder) bodyID() int {
	if r.main != nil {
		return r.main.id
	}
	return 1
}

// tap: click (web) or touch down / up (mobile) on a target at ts.
func (r *recorder) tap(ts int64, t target, count int) {
	vx, vy := t.b.cx()+rand.NormFloat64()*t.b.w/8, t.b.cy()+rand.NormFloat64()*t.b.h/8
	if !t.b.fixed {
		if vy-r.scrollY < 80 || vy-r.scrollY > r.H-60 {
			r.scrollTo(ts-350, vy-r.H/2)
		}
		vy -= r.scrollY
	}
	r.moveTo(ts-40, vx, vy)
	for i := 0; i < count; i++ {
		at := ts + int64(i*130)
		if r.web {
			down, up := miMouseDown, miMouseUp
			if r.touch {
				down, up = miTouchStart, miTouchEnd
			}
			for j, typ := range []int{down, up, miClick} {
				r.push(tsEvent{ts: at + int64(j*45), clicks: b2i(typ == miClick), ev: rrEvent{Type: rrIncr, Timestamp: at + int64(j*45),
					Data: map[string]any{"source": srcInteraction, "type": typ, "id": t.id, "x": math.Round(vx), "y": math.Round(vy)}}})
			}
		} else {
			r.push(tsEvent{ts: at, ev: map[string]any{"type": "touch", "ts": at, "x": math.Round(vx), "y": math.Round(vy), "phase": "down"}})
			r.push(tsEvent{ts: at + 85, clicks: 1, ev: map[string]any{"type": "touch", "ts": at + 85, "x": math.Round(vx), "y": math.Round(vy), "phase": "up"}})
		}
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// typeInto types n characters into input target t starting at ts (masked).
func (r *recorder) typeInto(ts int64, t target, idx, n int) int64 {
	r.tap(ts, t, 1)
	at := ts + 260
	if r.web {
		r.push(tsEvent{ts: at, ev: rrEvent{Type: rrIncr, Timestamp: at, Data: map[string]any{"source": srcInteraction, "type": miFocus, "id": t.id}}})
		for i := 1; i <= n; i++ {
			at += int64(90 + rand.IntN(160))
			r.push(tsEvent{ts: at, inputs: b2i(i == n), ev: rrEvent{Type: rrIncr, Timestamp: at, Data: map[string]any{
				"source": srcInput, "id": t.id, "text": strings.Repeat("*", i), "isChecked": false, "userTriggered": true}}})
		}
		at += 200
		r.push(tsEvent{ts: at, ev: rrEvent{Type: rrIncr, Timestamp: at, Data: map[string]any{"source": srcInteraction, "type": miBlur, "id": t.id}}})
		return at
	}
	r.push(tsEvent{ts: at, ev: map[string]any{"type": "keyboard", "ts": at, "visible": true}})
	end := at + int64(n*170)
	for f := at + minFrameGap; f <= end; f += minFrameGap {
		r.typed[idx] = int(float64(n) * float64(f-at) / float64(end-at))
		r.frameAt(f)
	}
	r.typed[idx] = n
	r.frameAt(end)
	r.push(tsEvent{ts: end + 300, inputs: 1, ev: map[string]any{"type": "keyboard", "ts": end + 300, "visible": false}})
	return end + 300
}

// interact mirrors a RUM ACTION: a click / tap on a plausible target (search actions type first).
func (r *recorder) interact(ts int64, name string, rage bool) {
	if len(r.targets) == 0 {
		return
	}
	if name == "Search" {
		for i, b := range r.content {
			if b.role == "search" {
				if t, ok := r.targetOf(b); ok {
					r.typeInto(ts, t, i, 5+rand.IntN(8))
					return
				}
			}
		}
	}
	var clicks []target
	for _, t := range r.targets {
		if t.b.role == "click" {
			clicks = append(clicks, t)
		}
	}
	if len(clicks) == 0 {
		return
	}
	t := clicks[rand.IntN(len(clicks))]
	n := 1
	if rage {
		n = 4
		r.push(tsEvent{ts: ts, rage: 1})
	}
	r.tap(ts, t, n)
}

func (r *recorder) targetOf(b box) (target, bool) {
	for _, t := range r.targets {
		if t.b == b {
			return t, true
		}
	}
	return target{}, false
}

// scheduleForm types into the inputs of a form page over the next seconds.
func (r *recorder) scheduleForm(ts int64) {
	at := ts + 1200 + int64(rand.IntN(800))
	for i, b := range r.content {
		if b.role != "input" && b.role != "password" {
			continue
		}
		t, ok := r.targetOf(b)
		if !ok {
			continue
		}
		at = r.typeInto(at, t, i, 4+rand.IntN(14)) + int64(300+rand.IntN(500))
	}
}

// ---------------------------------------------------------------- mobile (wireframe)

func (r *recorder) frame(ts int64) { r.frameAt(max(ts, r.lastFrame+minFrameGap)) }

func (r *recorder) frameAt(ts int64) {
	if ts < r.lastFrame+minFrameGap {
		ts = r.lastFrame + minFrameGap
	}
	r.lastFrame = ts
	nodes := []any{}
	r.targets = r.targets[:0]
	id := 0
	addNode := func(b box, y float64, typed int) {
		id++
		n := map[string]any{"id": id, "x": math.Round(b.x), "y": math.Round(y), "w": math.Round(b.w), "h": math.Round(b.h), "kind": b.kind}
		if b.bg != "" {
			n["bg"] = b.bg
		}
		if b.fg != "" {
			n["fg"] = b.fg
		}
		if b.border != "" {
			n["border"] = b.border
		}
		if b.radius > 0 {
			n["radius"] = b.radius
		}
		switch {
		case b.kind == "input":
			if typed > 0 {
				n["text"], n["masked"] = strings.Repeat("•", typed), true
			}
		case b.text != "" && b.unmask:
			n["text"] = b.text
		case b.text != "":
			n["text"], n["masked"] = maskText(b.text, '•'), true
		}
		nodes = append(nodes, n)
		if b.role != "" {
			r.targets = append(r.targets, target{b: b, id: id})
		}
	}
	addNode(box{kind: "view", x: 0, y: 0, w: r.W, h: r.H, bg: "#ffffff"}, 0, 0)
	top, bottom := 91.0, r.H-83
	for i, b := range r.content {
		y := b.y - r.scrollY
		if y+b.h < top || y > bottom {
			continue
		}
		addNode(b, y, r.typed[i])
	}
	for _, b := range r.chrome { // bars on top
		addNode(b, b.y, 0)
	}
	r.push(tsEvent{ts: ts, full: true, ev: map[string]any{"type": "frame", "ts": ts, "nodes": nodes}})
}

// swipe scrolls a mobile screen by dy with a touch gesture ending at ts.
func (r *recorder) swipe(ts int64, dy float64) {
	x := r.W*0.5 + rand.NormFloat64()*30
	y0 := r.H * 0.7
	start := ts - 420
	r.push(tsEvent{ts: start, ev: map[string]any{"type": "touch", "ts": start, "x": math.Round(x), "y": math.Round(y0), "phase": "down"}})
	for i := 1; i <= 3; i++ {
		at := start + int64(i*110)
		r.push(tsEvent{ts: at, ev: map[string]any{"type": "touch", "ts": at, "x": math.Round(x), "y": math.Round(y0 - dy*float64(i)/4), "phase": "move"}})
	}
	r.push(tsEvent{ts: ts, ev: map[string]any{"type": "touch", "ts": ts, "x": math.Round(x), "y": math.Round(y0 - dy), "phase": "up"}})
	r.scrollY = math.Max(0, math.Min(r.scrollY+dy, r.docH-r.H))
	r.frameAt(ts + 60)
}

// ---------------------------------------------------------------- idle activity between RUM events

// fill adds mouse movement / scrolling for (lastFill, now].
func (r *recorder) fill(now int64) {
	if r.viewID == "" {
		r.lastFill = now
		return
	}
	for t := r.lastFill + int64(400+rand.IntN(900)); t <= now; t += int64(500 + rand.IntN(1100)) {
		switch {
		case rand.Float64() < 0.12 && r.docH > r.H+40:
			dy := float64(150 + rand.IntN(450))
			if r.scrollY+r.H >= r.docH-10 || rand.Float64() < 0.2 {
				dy = -dy
			}
			if r.web {
				r.scrollTo(t, r.scrollY+dy)
			} else {
				r.swipe(t, dy)
			}
		case r.web && !r.touch:
			r.moveTo(t, math.Max(0, math.Min(r.W-1, r.mx+rand.NormFloat64()*160)), math.Max(0, math.Min(r.H-1, r.my+rand.NormFloat64()*110)))
		}
	}
	r.lastFill = now
}

// ---------------------------------------------------------------- segments

// cut takes the events up to now as one segment (nil when there are none).
func (r *recorder) cut(now int64) *segment {
	sort.SliceStable(r.evs, func(i, j int) bool { return r.evs[i].ts < r.evs[j].ts })
	n := sort.Search(len(r.evs), func(i int) bool { return r.evs[i].ts > now })
	if n == 0 {
		return nil
	}
	take := r.evs[:n]
	sg := &segment{start: take[0].ts, end: take[n-1].ts, viewID: take[0].viewID, viewName: take[0].viewName}
	for _, e := range take {
		sg.clicks += e.clicks
		sg.inputs += e.inputs
		sg.rage += e.rage
		sg.errs += e.errs
		sg.full = sg.full || e.full
		switch ev := e.ev.(type) {
		case rrEvent:
			ev.Timestamp = e.ts
			sg.events = append(sg.events, ev)
		case map[string]any:
			ev["ts"] = e.ts
			sg.events = append(sg.events, ev)
		}
	}
	r.evs = append([]tsEvent(nil), r.evs[n:]...)
	r.lastCut = now
	if len(sg.events) == 0 && sg.errs == 0 {
		return nil
	}
	if sg.events == nil {
		sg.events = []any{}
	}
	return sg
}

func (r *recorder) loop(ctx context.Context) {
	tick := time.NewTicker(segmentEvery)
	defer tick.Stop()
	for {
		final := false
		select {
		case <-ctx.Done():
			final = true
		case <-r.done:
			final = true
		case <-tick.C:
		case <-r.wake:
		}
		r.mu.Lock()
		now := r.now()
		if final {
			r.evs = slicesFilter(r.evs, func(e tsEvent) bool { return e.ts <= now }) // scheduled but never happened
		}
		r.fill(now)
		sg := r.cut(now)
		var send []segment
		switch {
		case r.mode == modeRecording:
			if r.errFlush {
				send = append(send, r.buf...)
				r.buf, r.errFlush = nil, false
			}
			if sg != nil {
				send = append(send, *sg)
			}
		default: // buffering: keep 30–60 s, take a checkout snapshot every 30 s
			if sg != nil {
				r.buf = append(r.buf, *sg)
			}
			if r.lastCkpt > 0 && now-r.lastCkpt >= checkoutEvery.Milliseconds() && !final {
				keepFrom := r.lastCkpt
				r.buf = slicesFilter(r.buf, func(s segment) bool { return s.end >= keepFrom })
				r.checkout(now)
			}
		}
		first := r.segNo
		r.segNo += len(send)
		r.mu.Unlock()
		for i, s := range send {
			r.send(ctx, first+i, s)
		}
		if final {
			return
		}
	}
}

// checkout re-snapshots the current screen (buffer mode).
func (r *recorder) checkout(now int64) {
	if r.web {
		if r.doc != nil {
			r.push(tsEvent{ts: now, ev: rrEvent{Type: rrMeta, Timestamp: now, Data: map[string]any{"href": viewURL(r.s.app, r.viewName), "width": r.W, "height": r.H}}})
			r.push(tsEvent{ts: now, full: true, ev: rrEvent{Type: rrFull, Timestamp: now, Data: map[string]any{
				"node": r.doc.ser(true), "initialOffset": map[string]any{"left": 0, "top": r.scrollY}}}})
		}
	} else {
		r.push(tsEvent{ts: now, ev: map[string]any{"type": "screen", "ts": now, "width": r.W, "height": r.H, "density": r.density, "name": screenTitle(r.viewName) + "Screen"}})
		r.lastFrame = 0
		r.frameAt(now)
	}
	r.lastCkpt = now
}

func slicesFilter[T any](xs []T, keep func(T) bool) []T {
	out := xs[:0]
	for _, x := range xs {
		if keep(x) {
			out = append(out, x)
		}
	}
	return out
}

// payload builds the POST /v1/replay body of a segment.
func (r *recorder) payload(no int, sg segment) map[string]any {
	a := r.s.app
	platform, format, sdk, sdkVer := "web", "rrweb", replaySDK, replaySDKVer
	if a.mobile {
		format, sdk, sdkVer = "wireframe", a.sdk, a.sdkVersion
		platform = "ios"
		if r.s.dev.osName == "Android" {
			platform = "android"
		}
	}
	return map[string]any{
		"resource": map[string]any{"service": a.name, "env": world.Environment, "version": a.version, "platform": platform,
			"sdk_name": sdk, "sdk_version": sdkVer},
		"session_id": r.s.id, "segment": no, "start_ts": sg.start, "end_ts": sg.end, "format": format,
		"has_full_snapshot": sg.full, "view_id": sg.viewID, "view_name": sg.viewName,
		"user_id": r.s.userID, "anonymous_id": r.s.anon,
		"stats":  map[string]any{"clicks": sg.clicks, "errors": sg.errs, "rage_clicks": sg.rage, "inputs": sg.inputs},
		"events": sg.events,
	}
}

func (r *recorder) send(ctx context.Context, no int, sg segment) {
	body, err := json.Marshal(r.payload(no, sg))
	if err != nil {
		r.t.ReplayErr.Add(1)
		return
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(body)
	_ = zw.Close()
	req, err := http.NewRequestWithContext(context.WithoutCancel(ctx), "POST", r.t.IngestURL+"/v1/replay", &buf)
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("x-trooper-key", r.t.IngestKey)
	req.Header.Set("User-Agent", r.s.dev.ua)
	resp, err := r.t.rum.Do(req)
	if err != nil {
		r.t.ReplayErr.Add(1)
		return
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		if r.t.ReplayErr.Add(1)%50 == 1 {
			r.t.Log.Warn("replay segment rejected", "status", resp.StatusCode, "body", strings.TrimSpace(string(msg)))
		}
		return
	}
	if no == 0 {
		r.t.ReplaySessions.Add(1)
	}
	r.t.ReplaySegs.Add(1)
}
