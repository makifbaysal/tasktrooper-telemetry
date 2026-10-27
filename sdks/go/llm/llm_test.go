package llm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/tasktrooper/telemetry-go/llm"
	"github.com/tasktrooper/telemetry-go/troopertest"
)

// fakeAPI is a base RoundTripper replaying a recorded response and recording the request body.
type fakeAPI struct {
	mu          sync.Mutex
	status      int
	contentType string
	body        []byte
	// firstChunk, when > 0, sends the first firstChunk bytes, waits delay, then the rest.
	firstChunk int
	delay      time.Duration
	err        error

	gotBody []byte
	gotReq  *http.Request
}

func (f *fakeAPI) RoundTrip(req *http.Request) (*http.Response, error) {
	var b []byte
	if req.Body != nil {
		b, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	f.mu.Lock()
	f.gotBody, f.gotReq = b, req
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var body io.ReadCloser = io.NopCloser(bytes.NewReader(f.body))
	if f.firstChunk > 0 {
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write(f.body[:f.firstChunk])
			time.Sleep(f.delay)
			_, _ = pw.Write(f.body[f.firstChunk:])
			_ = pw.Close()
		}()
		body = pr
	}
	status := f.status
	if status == 0 {
		status = 200
	}
	h := http.Header{}
	h.Set("Content-Type", f.contentType)
	return &http.Response{StatusCode: status, Header: h, Body: body, Request: req}, nil
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func attr(s sdktrace.ReadOnlySpan, k attribute.Key) (attribute.Value, bool) {
	return troopertest.Attr(s, k)
}

func wantInt(t *testing.T, s sdktrace.ReadOnlySpan, k attribute.Key, want int64) {
	t.Helper()
	v, ok := attr(s, k)
	if !ok || v.AsInt64() != want {
		t.Errorf("%s = %v (present %v), want %d", k, v.Emit(), ok, want)
	}
}

func wantStr(t *testing.T, s sdktrace.ReadOnlySpan, k attribute.Key, want string) {
	t.Helper()
	v, ok := attr(s, k)
	if !ok || v.Emit() != want {
		t.Errorf("%s = %q (present %v), want %q", k, v.Emit(), ok, want)
	}
}

func llmSpan(t *testing.T, h *troopertest.Harness) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range h.Spans() {
		if _, ok := attr(s, llm.AttrOperation); ok {
			return s
		}
	}
	t.Fatalf("no LLM span among %d spans", len(h.Spans()))
	return nil
}

func TestTransportProviders(t *testing.T) {
	tests := []struct {
		name        string
		url         string
		reqBody     string
		fixture     string
		contentType string
		wantName    string
		wantProv    string
		wantOp      string
		wantIn      int64
		wantOut     int64
		wantCacheR  int64
		wantCacheW  int64
		wantRespMod string
		wantFinish  string
		wantMaxTok  int64
		wantText    string // captured output text
		stream      bool
	}{
		{
			name: "openai chat", url: "https://api.openai.com/v1/chat/completions",
			reqBody: `{"model":"gpt-4o","max_tokens":256,"temperature":0.2,"messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"Hi"}]}`,
			fixture: "openai_chat.json", contentType: "application/json",
			wantName: "chat gpt-4o", wantProv: "openai", wantOp: "chat", wantIn: 19, wantOut: 3, wantCacheR: 8,
			wantRespMod: "gpt-4o-2024-08-06", wantFinish: "stop", wantMaxTok: 256, wantText: "Hello there!",
		},
		{
			name: "openai chat stream", url: "https://api.openai.com/v1/chat/completions",
			reqBody: `{"model":"gpt-4o-mini","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":[{"type":"text","text":"Hi"}]}]}`,
			fixture: "openai_chat_stream.sse", contentType: "text/event-stream; charset=utf-8",
			wantName: "chat gpt-4o-mini", wantProv: "openai", wantOp: "chat", wantIn: 10, wantOut: 2, wantCacheR: 4,
			wantRespMod: "gpt-4o-mini-2024-07-18", wantFinish: "stop", wantText: "Hello!", stream: true,
		},
		{
			name: "openai responses stream", url: "https://api.openai.com/v1/responses",
			reqBody: `{"model":"gpt-4.1","stream":true,"instructions":"Be nice","input":"Hi","max_output_tokens":100}`,
			fixture: "openai_responses_stream.sse", contentType: "text/event-stream",
			wantName: "chat gpt-4.1", wantProv: "openai", wantOp: "chat", wantIn: 36, wantOut: 2,
			wantRespMod: "gpt-4.1-2025-04-14", wantFinish: "completed", wantMaxTok: 100, wantText: "Hi", stream: true,
		},
		{
			name: "anthropic messages", url: "https://api.anthropic.com/v1/messages",
			reqBody: `{"model":"claude-sonnet-4-5","max_tokens":1024,"system":"You are terse.","messages":[{"role":"user","content":"Weather in Paris?"}]}`,
			fixture: "anthropic_messages.json", contentType: "application/json",
			wantName: "chat claude-sonnet-4-5", wantProv: "anthropic", wantOp: "chat", wantIn: 162, wantOut: 5, wantCacheR: 50, wantCacheW: 100,
			wantRespMod: "claude-sonnet-4-5-20250929", wantFinish: "tool_use", wantMaxTok: 1024, wantText: "Hi!",
		},
		{
			name: "anthropic stream", url: "https://api.anthropic.com/v1/messages",
			reqBody: `{"model":"claude-haiku-4-5","max_tokens":512,"stream":true,"system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":[{"type":"text","text":"Hello?"}]}]}`,
			fixture: "anthropic_stream.sse", contentType: "text/event-stream",
			wantName: "chat claude-haiku-4-5", wantProv: "anthropic", wantOp: "chat", wantIn: 35, wantOut: 15, wantCacheR: 10,
			wantRespMod: "claude-haiku-4-5-20251001", wantFinish: "end_turn", wantMaxTok: 512, wantText: "Hello world", stream: true,
		},
		{
			name: "gemini", url: "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash:generateContent",
			reqBody: `{"contents":[{"role":"user","parts":[{"text":"Say hello in French"}]}],"systemInstruction":{"parts":[{"text":"sys"}]},"generationConfig":{"maxOutputTokens":64,"temperature":0.5}}`,
			fixture: "gemini.json", contentType: "application/json; charset=UTF-8",
			wantName: "generate_content gemini-2.0-flash", wantProv: "gcp.gemini", wantOp: "generate_content", wantIn: 7, wantOut: 2, wantCacheR: 3,
			wantRespMod: "gemini-2.0-flash", wantFinish: "STOP", wantMaxTok: 64, wantText: "Bonjour",
		},
		{
			name: "gemini stream", url: "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse",
			reqBody: `{"contents":[{"role":"user","parts":[{"text":"Say hello in French"}]}]}`,
			fixture: "gemini_stream.sse", contentType: "text/event-stream",
			wantName: "generate_content gemini-2.5-flash", wantProv: "gcp.gemini", wantOp: "generate_content", wantIn: 7, wantOut: 6,
			wantRespMod: "gemini-2.5-flash", wantFinish: "STOP", wantText: "Bonjour", stream: true,
		},
		{
			name: "azure openai deployment", url: "https://acme.openai.azure.com/openai/deployments/my-gpt4o/chat/completions?api-version=2024-10-21",
			reqBody: `{"messages":[{"role":"user","content":"Hi"}]}`,
			fixture: "openai_chat.json", contentType: "application/json",
			wantName: "chat my-gpt4o", wantProv: "azure.ai.openai", wantOp: "chat", wantIn: 19, wantOut: 3, wantCacheR: 8,
			wantRespMod: "gpt-4o-2024-08-06", wantFinish: "stop", wantText: "Hello there!",
		},
		{
			name: "groq openai-compatible", url: "https://api.groq.com/openai/v1/chat/completions",
			reqBody: `{"model":"llama-3.3-70b-versatile","messages":[{"role":"user","content":"Hi"}]}`,
			fixture: "openai_chat.json", contentType: "application/json",
			wantName: "chat llama-3.3-70b-versatile", wantProv: "groq", wantOp: "chat", wantIn: 19, wantOut: 3, wantCacheR: 8,
			wantRespMod: "gpt-4o-2024-08-06", wantFinish: "stop", wantText: "Hello there!",
		},
	}
	for _, tt := range tests {
		for _, captureOn := range []bool{false, true} {
			name := tt.name + "/capture=" + map[bool]string{false: "off", true: "on"}[captureOn]
			t.Run(name, func(t *testing.T) {
				h := troopertest.New(t, "svc")
				body := fixture(t, tt.fixture)
				api := &fakeAPI{contentType: tt.contentType, body: body}
				if tt.stream {
					api.firstChunk, api.delay = bytes.Index(body, []byte("\n\n"))+2, 20*time.Millisecond
				}
				client := llm.HTTPClient(&http.Client{Transport: api}, llm.WithCaptureContent(captureOn))

				ctx, parent := h.Provider.Tracer("t").Start(context.Background(), "parent")
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, tt.url, strings.NewReader(tt.reqBody))
				req.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				parent.End()
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, body) {
					t.Fatalf("response body altered:\n%q\nwant\n%q", got, body)
				}
				if string(api.gotBody) != tt.reqBody {
					t.Fatalf("request body altered: %q", api.gotBody)
				}
				if !trace.SpanContextFromContext(api.gotReq.Context()).IsValid() {
					t.Error("base transport did not get the LLM span context")
				}

				s := llmSpan(t, h)
				if s.Name() != tt.wantName {
					t.Errorf("name = %q, want %q", s.Name(), tt.wantName)
				}
				if s.SpanKind() != trace.SpanKindClient {
					t.Errorf("kind = %v", s.SpanKind())
				}
				if s.Parent().SpanID() != parent.SpanContext().SpanID() {
					t.Error("LLM span is not a child of the caller's span")
				}
				wantStr(t, s, llm.AttrProvider, tt.wantProv)
				wantStr(t, s, llm.AttrOperation, tt.wantOp)
				wantInt(t, s, llm.AttrInputTokens, tt.wantIn)
				wantInt(t, s, llm.AttrOutputTokens, tt.wantOut)
				if tt.wantCacheR > 0 {
					wantInt(t, s, llm.AttrCacheReadTokens, tt.wantCacheR)
				}
				if tt.wantCacheW > 0 {
					wantInt(t, s, llm.AttrCacheWriteTokens, tt.wantCacheW)
				}
				if tt.wantMaxTok > 0 {
					wantInt(t, s, llm.AttrRequestMaxTokens, tt.wantMaxTok)
				}
				wantStr(t, s, llm.AttrResponseModel, tt.wantRespMod)
				if v, _ := attr(s, llm.AttrFinishReasons); len(v.AsStringSlice()) == 0 || v.AsStringSlice()[0] != tt.wantFinish {
					t.Errorf("finish reasons = %v, want [%s]", v.AsStringSlice(), tt.wantFinish)
				}
				ttft, hasTTFT := attr(s, llm.AttrTTFT)
				if tt.stream {
					if !hasTTFT || ttft.AsFloat64() < 15 {
						t.Errorf("ttft = %v (present %v), want >= 15ms (first content after the delayed chunk)", ttft.AsFloat64(), hasTTFT)
					}
				} else if hasTTFT {
					t.Error("ttft recorded for a non-streaming call")
				}
				if s.Status().Code == codes.Error {
					t.Errorf("unexpected error status %v", s.Status())
				}

				in, hasIn := attr(s, llm.AttrInputMessages)
				out, hasOut := attr(s, llm.AttrOutputMessages)
				if !captureOn {
					if hasIn || hasOut {
						t.Error("content recorded with capture off")
					}
					return
				}
				if !hasIn || !hasOut {
					t.Fatalf("content not recorded (in %v, out %v)", hasIn, hasOut)
				}
				var inMsgs, outMsgs []llm.Message
				if err := json.Unmarshal([]byte(in.AsString()), &inMsgs); err != nil || len(inMsgs) == 0 {
					t.Fatalf("input messages %q: %v", in.AsString(), err)
				}
				if err := json.Unmarshal([]byte(out.AsString()), &outMsgs); err != nil || len(outMsgs) != 1 {
					t.Fatalf("output messages %q: %v", out.AsString(), err)
				}
				if p := outMsgs[0].Parts; len(p) == 0 || p[0].Content != tt.wantText {
					t.Errorf("output = %s, want text %q", out.AsString(), tt.wantText)
				}
			})
		}
	}
}

func TestTransportSystemInstructions(t *testing.T) {
	h := troopertest.New(t, "svc")
	api := &fakeAPI{contentType: "application/json", body: fixture(t, "anthropic_messages.json")}
	client := llm.HTTPClient(&http.Client{Transport: api}, llm.WithCaptureContent(true))
	resp, err := client.Post("https://api.anthropic.com/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":10,"system":"You are terse.","messages":[{"role":"user","content":"Hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	s := llmSpan(t, h)
	wantStr(t, s, llm.AttrSystemInstructions, `[{"type":"text","content":"You are terse."}]`)
	out, _ := attr(s, llm.AttrOutputMessages)
	if !strings.Contains(out.AsString(), `"type":"tool_call","id":"toolu_1","name":"get_weather"`) {
		t.Errorf("tool call missing from output: %s", out.AsString())
	}
}

func TestTransportPassthrough(t *testing.T) {
	h := troopertest.New(t, "svc")
	api := &fakeAPI{contentType: "application/json", body: []byte(`{"ok":true}`)}
	client := llm.HTTPClient(&http.Client{Transport: api})
	for _, u := range []string{
		"https://example.com/v1/chat/completions", // unknown host
		"https://api.openai.com/v1/models",        // known host, other API
	} {
		req, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(`{"model":"x"}`))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(b) != `{"ok":true}` {
			t.Fatalf("body = %q", b)
		}
		if api.gotReq != req {
			t.Errorf("%s: request was not passed through untouched", u)
		}
	}
	if n := len(h.Spans()); n != 0 {
		t.Fatalf("got %d spans for pass-through requests", n)
	}
}

func TestTransportCustomProvider(t *testing.T) {
	h := troopertest.New(t, "svc")
	api := &fakeAPI{contentType: "application/json", body: fixture(t, "openai_chat.json")}
	client := llm.HTTPClient(&http.Client{Transport: api}, llm.WithProvider("localhost:11434", "ollama"))
	resp, err := client.Post("http://localhost:11434/v1/chat/completions", "application/json", strings.NewReader(`{"model":"llama3.2"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	s := llmSpan(t, h)
	wantStr(t, s, llm.AttrProvider, "ollama")
	wantStr(t, s, llm.AttrRequestModel, "llama3.2")
	wantInt(t, s, llm.AttrInputTokens, 19)
}

func TestTransportErrors(t *testing.T) {
	t.Run("http 429", func(t *testing.T) {
		h := troopertest.New(t, "svc")
		api := &fakeAPI{status: 429, contentType: "application/json", body: []byte(`{"error":{"type":"rate_limit_error"}}`)}
		client := llm.HTTPClient(&http.Client{Transport: api})
		resp, err := client.Post("https://api.anthropic.com/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x"}`))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 429 || string(b) != `{"error":{"type":"rate_limit_error"}}` {
			t.Fatalf("response altered: %d %q", resp.StatusCode, b)
		}
		s := llmSpan(t, h)
		if s.Status().Code != codes.Error {
			t.Error("status not error")
		}
		wantStr(t, s, llm.AttrErrorType, "429")
	})
	t.Run("network error", func(t *testing.T) {
		h := troopertest.New(t, "svc")
		boom := errors.New("dial failed")
		client := llm.HTTPClient(&http.Client{Transport: &fakeAPI{err: boom}})
		_, err := client.Post("https://api.openai.com/v1/chat/completions", "application/json", strings.NewReader(`{"model":"gpt-4o"}`))
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		s := llmSpan(t, h)
		if s.Status().Code != codes.Error {
			t.Error("status not error")
		}
	})
	t.Run("garbage bodies", func(t *testing.T) {
		h := troopertest.New(t, "svc")
		body := []byte("data: {not json\n\ndata: [DONE]\n\n")
		api := &fakeAPI{contentType: "text/event-stream", body: body}
		client := llm.HTTPClient(&http.Client{Transport: api})
		reqBody := `{"model": [1,2` // invalid JSON
		resp, err := client.Post("https://api.openai.com/v1/chat/completions", "application/json", strings.NewReader(reqBody))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !bytes.Equal(b, body) || string(api.gotBody) != reqBody {
			t.Fatal("bodies altered")
		}
		s := llmSpan(t, h)
		if s.Name() != "chat" {
			t.Errorf("name = %q", s.Name())
		}
		if _, ok := attr(s, llm.AttrInputTokens); ok {
			t.Error("tokens recorded from garbage")
		}
	})
	t.Run("close before EOF ends span", func(t *testing.T) {
		h := troopertest.New(t, "svc")
		api := &fakeAPI{contentType: "text/event-stream", body: fixture(t, "anthropic_stream.sse")}
		client := llm.HTTPClient(&http.Client{Transport: api})
		resp, err := client.Post("https://api.anthropic.com/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x","stream":true}`))
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 10)
		_, _ = resp.Body.Read(buf)
		_ = resp.Body.Close()
		_ = llmSpan(t, h)
	})
}

func TestStartEnd(t *testing.T) {
	h := troopertest.New(t, "svc")
	llm.SetCaptureContent(true)
	t.Cleanup(llm.ResetCaptureContent)

	ctx, call := llm.Start(context.Background(), llm.Request{
		Provider: "anthropic", Model: "claude-sonnet-4-5", MaxTokens: 100, Temperature: 0.7,
		SystemInstructions: []llm.Part{{Type: "text", Content: "be brief"}},
		InputMessages:      []llm.Message{llm.Text("user", "hi")},
	})
	if !trace.SpanContextFromContext(ctx).IsValid() {
		t.Fatal("ctx carries no span")
	}
	time.Sleep(5 * time.Millisecond)
	call.FirstToken()
	call.FirstToken() // only the first counts
	call.End(llm.Usage{
		InputTokens: 10, OutputTokens: 20, CacheReadTokens: 3, ResponseModel: "claude-sonnet-4-5-20250929",
		ResponseID: "msg_1", FinishReasons: []string{"end_turn"},
		OutputMessages: []llm.Message{llm.Text("assistant", "hello")},
	})
	call.End(llm.Usage{InputTokens: 999}) // ignored

	s := llmSpan(t, h)
	if s.Name() != "chat claude-sonnet-4-5" || s.SpanKind() != trace.SpanKindClient {
		t.Errorf("span %q kind %v", s.Name(), s.SpanKind())
	}
	wantStr(t, s, llm.AttrProvider, "anthropic")
	wantInt(t, s, llm.AttrRequestMaxTokens, 100)
	wantInt(t, s, llm.AttrInputTokens, 10)
	wantInt(t, s, llm.AttrOutputTokens, 20)
	wantInt(t, s, llm.AttrCacheReadTokens, 3)
	wantStr(t, s, llm.AttrResponseID, "msg_1")
	wantStr(t, s, llm.AttrInputMessages, `[{"role":"user","parts":[{"type":"text","content":"hi"}]}]`)
	wantStr(t, s, llm.AttrOutputMessages, `[{"role":"assistant","parts":[{"type":"text","content":"hello"}]}]`)
	wantStr(t, s, llm.AttrSystemInstructions, `[{"type":"text","content":"be brief"}]`)
	if v, ok := attr(s, llm.AttrTTFT); !ok || v.AsFloat64() < 4 {
		t.Errorf("ttft = %v", v.AsFloat64())
	}
	if v, _ := attr(s, llm.AttrRequestTemperature); v.AsFloat64() != 0.7 {
		t.Errorf("temperature = %v", v.AsFloat64())
	}

	// error + capture off via env default
	llm.SetCaptureContent(false)
	_, call = llm.Start(context.Background(), llm.Request{Provider: "openai", Model: "gpt-4o", InputMessages: []llm.Message{llm.Text("user", "secret")}})
	call.End(llm.Usage{Err: errors.New("boom")})
	var errSpan sdktrace.ReadOnlySpan
	for _, sp := range h.Spans() {
		if sp.Name() == "chat gpt-4o" {
			errSpan = sp
		}
	}
	if errSpan == nil || errSpan.Status().Code != codes.Error {
		t.Fatal("error span missing")
	}
	if _, ok := attr(errSpan, llm.AttrInputMessages); ok {
		t.Error("content captured while disabled")
	}
	if _, ok := attr(errSpan, llm.AttrErrorType); !ok {
		t.Error("error.type missing")
	}

	// nil call is safe
	var nilCall *llm.Call
	nilCall.FirstToken()
	nilCall.End(llm.Usage{})
}

func TestCaptureContentEnv(t *testing.T) {
	llm.ResetCaptureContent()
	t.Cleanup(llm.ResetCaptureContent)
	t.Setenv("TROOPER_LLM_CAPTURE_CONTENT", "true")
	if !llm.CaptureContent() {
		t.Error("env not honoured")
	}
	llm.SetCaptureContent(false)
	if llm.CaptureContent() {
		t.Error("SetCaptureContent(false) did not override env")
	}
}
