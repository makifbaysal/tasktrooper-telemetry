package llm

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/tasktrooper/telemetry-go/internal/sdkerr"
)

// API dialects understood by the transport.
type dialect int

const (
	dialectOpenAI    dialect = iota // OpenAI and OpenAI-compatible (Azure, Mistral, Groq, DeepSeek, xAI, OpenRouter …)
	dialectAnthropic                // Anthropic Messages API
	dialectGemini                   // Google Gemini (generativelanguage / Vertex AI generateContent)
)

type providerInfo struct {
	name    string
	dialect dialect
}

// knownHosts maps API hosts to providers.
var knownHosts = map[string]providerInfo{
	"api.openai.com":                    {"openai", dialectOpenAI},
	"api.anthropic.com":                 {"anthropic", dialectAnthropic},
	"generativelanguage.googleapis.com": {"gcp.gemini", dialectGemini},
	"aiplatform.googleapis.com":         {"gcp.vertex_ai", dialectGemini},
	"api.mistral.ai":                    {"mistral_ai", dialectOpenAI},
	"api.groq.com":                      {"groq", dialectOpenAI},
	"api.deepseek.com":                  {"deepseek", dialectOpenAI},
	"api.x.ai":                          {"x_ai", dialectOpenAI},
	"openrouter.ai":                     {"openrouter", dialectOpenAI},
	"api.together.xyz":                  {"together_ai", dialectOpenAI},
	"api.fireworks.ai":                  {"fireworks_ai", dialectOpenAI},
	"api.perplexity.ai":                 {"perplexity", dialectOpenAI},
	"api.cerebras.ai":                   {"cerebras", dialectOpenAI},
}

// knownSuffixes maps host suffixes to providers.
var knownSuffixes = []struct {
	suffix string
	info   providerInfo
}{
	{".openai.azure.com", providerInfo{"azure.ai.openai", dialectOpenAI}},
	{".cognitiveservices.azure.com", providerInfo{"azure.ai.openai", dialectOpenAI}},
	{".services.ai.azure.com", providerInfo{"azure.ai.openai", dialectOpenAI}},
	{"-aiplatform.googleapis.com", providerInfo{"gcp.vertex_ai", dialectGemini}},
}

// Option configures Transport / HTTPClient.
type Option func(*transport)

// WithProvider registers an extra API host (e.g. "llm.internal:8000", "localhost:11434") with its
// gen_ai.provider.name. Hosts are matched as host:port first, then host name. Providers
// "anthropic" use the Anthropic dialect, "gcp.gemini"/"gcp.vertex_ai" the Gemini dialect, all
// others the OpenAI-compatible dialect (/chat/completions, /completions, /embeddings, /responses).
func WithProvider(host, provider string) Option {
	return func(t *transport) {
		d := dialectOpenAI
		switch provider {
		case "anthropic":
			d = dialectAnthropic
		case "gcp.gemini", "gcp.vertex_ai":
			d = dialectGemini
		}
		t.hosts[strings.ToLower(host)] = providerInfo{provider, d}
	}
}

// WithCaptureContent enables or disables recording of prompts/completions for this transport,
// overriding the process-wide setting.
func WithCaptureContent(on bool) Option {
	return func(t *transport) { t.capture = &on }
}

// maxBody bounds request/response bytes buffered for parsing; larger bodies pass through
// untouched without token data.
const maxBody = 8 << 20

type transport struct {
	base    http.RoundTripper
	hosts   map[string]providerInfo
	capture *bool
}

// Transport wraps base (http.DefaultTransport when nil) so requests to known LLM APIs produce
// GenAI client spans. Requests to other hosts and other API paths pass through untouched.
func Transport(base http.RoundTripper, opts ...Option) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	t := &transport{base: base, hosts: map[string]providerInfo{}}
	for _, o := range opts {
		if o != nil {
			o(t)
		}
	}
	return t
}

// HTTPClient returns a copy of base (a new client when nil) whose Transport is wrapped with
// Transport.
func HTTPClient(base *http.Client, opts ...Option) *http.Client {
	c := &http.Client{}
	if base != nil {
		cp := *base
		c = &cp
	}
	c.Transport = Transport(c.Transport, opts...)
	return c
}

func (t *transport) provider(host, hostname string) (providerInfo, bool) {
	host, hostname = strings.ToLower(host), strings.ToLower(hostname)
	if p, ok := t.hosts[host]; ok {
		return p, true
	}
	if p, ok := t.hosts[hostname]; ok {
		return p, true
	}
	if p, ok := knownHosts[hostname]; ok {
		return p, true
	}
	for _, s := range knownSuffixes {
		if strings.HasSuffix(hostname, s.suffix) {
			return s.info, true
		}
	}
	return providerInfo{}, false
}

// endpoint describes a recognised API call.
type endpoint struct {
	provider  providerInfo
	operation string
	model     string // from the URL (Gemini, Azure deployment)
	stream    bool   // from the URL (Gemini streamGenerateContent)
}

func (t *transport) match(req *http.Request) (endpoint, bool) {
	if req.Method != http.MethodPost || req.URL == nil {
		return endpoint{}, false
	}
	p, ok := t.provider(req.URL.Host, req.URL.Hostname())
	if !ok {
		return endpoint{}, false
	}
	path := strings.TrimRight(req.URL.Path, "/")
	e := endpoint{provider: p}
	switch p.dialect {
	case dialectAnthropic:
		if !strings.HasSuffix(path, "/messages") {
			return e, false
		}
		e.operation = OperationChat
	case dialectGemini:
		i := strings.LastIndex(path, "/models/")
		if i < 0 {
			return e, false
		}
		rest := path[i+len("/models/"):]
		j := strings.LastIndexByte(rest, ':')
		if j < 0 {
			return e, false
		}
		e.model = rest[:j]
		switch rest[j+1:] {
		case "generateContent":
			e.operation = OperationGenerateContent
		case "streamGenerateContent":
			e.operation, e.stream = OperationGenerateContent, true
		case "embedContent", "batchEmbedContents":
			e.operation = OperationEmbeddings
		default:
			return e, false
		}
	default:
		switch {
		case strings.HasSuffix(path, "/chat/completions"):
			e.operation = OperationChat
		case strings.HasSuffix(path, "/completions"):
			e.operation = OperationTextCompletion
		case strings.HasSuffix(path, "/embeddings"):
			e.operation = OperationEmbeddings
		case strings.HasSuffix(path, "/responses"):
			e.operation = OperationChat
		default:
			return e, false
		}
		// Azure OpenAI: /openai/deployments/<deployment>/chat/completions
		if i := strings.Index(path, "/deployments/"); i >= 0 {
			rest := path[i+len("/deployments/"):]
			if j := strings.IndexByte(rest, '/'); j > 0 {
				e.model = rest[:j]
			}
		}
	}
	return e, true
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ep, ok := t.safeMatch(req)
	if !ok {
		return t.base.RoundTrip(req)
	}
	captureContent := CaptureContent()
	if t.capture != nil {
		captureContent = *t.capture
	}

	// Read (a bounded prefix of) the request body and hand the base transport an identical stream.
	var reqBody []byte
	out := req
	if req.Body != nil && req.Body != http.NoBody && req.ContentLength <= maxBody &&
		req.Header.Get("Content-Encoding") == "" {
		buf, err := io.ReadAll(io.LimitReader(req.Body, maxBody+1))
		orig := req.Body
		out = req.Clone(req.Context())
		out.Body = &replayBody{Reader: io.MultiReader(bytes.NewReader(buf), orig), closer: orig}
		if err == nil && len(buf) <= maxBody {
			reqBody = buf
		}
	}

	info := parseRequest(ep, reqBody, captureContent)
	r := Request{
		Provider:           ep.provider.name,
		Model:              info.model,
		Operation:          ep.operation,
		MaxTokens:          info.maxTokens,
		Temperature:        info.temperature,
		TopP:               info.topP,
		ServerAddress:      req.URL.Hostname(),
		SystemInstructions: info.system,
		InputMessages:      info.input,
	}
	if p, err := strconv.Atoi(req.URL.Port()); err == nil {
		r.ServerPort = p
	} else if req.URL.Scheme == "https" {
		r.ServerPort = 443
	}
	ctx, call := start(req.Context(), r, captureContent)
	if out == req {
		out = req.WithContext(ctx)
	} else {
		out = out.WithContext(ctx)
	}

	resp, err := t.base.RoundTrip(out)
	if err != nil {
		call.End(Usage{Err: err})
		return resp, err
	}
	if resp == nil {
		call.End(Usage{})
		return resp, err
	}
	call.Span().SetAttributes(httpStatusAttr(resp.StatusCode))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		call.End(Usage{ErrorType: strconv.Itoa(resp.StatusCode)})
		return resp, nil
	}
	if resp.Body == nil || resp.Body == http.NoBody || req.Method == http.MethodHead {
		call.End(Usage{})
		return resp, nil
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		call.End(Usage{}) // compressed body we did not ask the transport to decode: pass through
		return resp, nil
	}
	acc := newAccumulator(ep.provider.dialect, call, captureContent)
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	var sink func([]byte)
	var finish func()
	if strings.HasPrefix(ct, "text/event-stream") {
		sse := &sseParser{onEvent: acc.event}
		sink, finish = sse.write, sse.flush
	} else {
		jb := &jsonBuffer{}
		sink = jb.write
		finish = func() {
			if !jb.overflow {
				acc.document(jb.buf.Bytes())
			}
		}
	}
	resp.Body = &tapBody{rc: resp.Body, sink: sink, finish: func(readErr error) {
		safe(finish)
		u := acc.usage()
		if readErr != nil {
			u.Err = readErr
		}
		call.End(u)
	}}
	return resp, nil
}

func (t *transport) safeMatch(req *http.Request) (ep endpoint, ok bool) {
	defer func() {
		if recover() != nil {
			sdkerr.Count("llm")
			ok = false
		}
	}()
	if req == nil {
		return endpoint{}, false
	}
	return t.match(req)
}

func safe(f func()) {
	defer func() {
		if recover() != nil {
			sdkerr.Count("llm")
		}
	}()
	if f != nil {
		f()
	}
}

// replayBody replays the buffered prefix followed by the rest of the original body; Close closes
// the original body.
type replayBody struct {
	io.Reader
	closer io.Closer
}

func (b *replayBody) Close() error { return b.closer.Close() }

// tapBody passes the response body through unchanged while feeding a copy of the bytes to sink;
// finish is called once on EOF, read error or Close. Read and Close may race (e.g. a stream
// cancelled from another goroutine), so the tap state is guarded by mu.
type tapBody struct {
	rc     io.ReadCloser
	sink   func([]byte)
	finish func(error)
	mu     sync.Mutex
	done   bool
	broken bool
}

func (b *tapBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.mu.Lock()
	if n > 0 && !b.broken && !b.done {
		b.feed(p[:n])
	}
	if err != nil && !b.done {
		b.done = true
		var readErr error
		if err != io.EOF {
			readErr = err
		}
		b.end(readErr)
	}
	b.mu.Unlock()
	return n, err
}

func (b *tapBody) feed(p []byte) {
	defer func() {
		if recover() != nil {
			sdkerr.Count("llm")
			b.broken = true
		}
	}()
	b.sink(p)
}

func (b *tapBody) end(err error) {
	defer func() {
		if recover() != nil {
			sdkerr.Count("llm")
		}
	}()
	b.finish(err)
}

func (b *tapBody) Close() error {
	err := b.rc.Close()
	b.mu.Lock()
	if !b.done {
		b.done = true
		b.end(nil)
	}
	b.mu.Unlock()
	return err
}

type jsonBuffer struct {
	buf      bytes.Buffer
	overflow bool
}

func (j *jsonBuffer) write(p []byte) {
	if j.overflow {
		return
	}
	if j.buf.Len()+len(p) > maxBody {
		j.overflow = true
		j.buf = bytes.Buffer{}
		return
	}
	j.buf.Write(p)
}
