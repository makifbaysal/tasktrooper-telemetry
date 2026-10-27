package profsim

// The synthetic code of the demo world: per service, the routes (or consumer processes) with the
// stacks their CPU time and allocations go to, plus background goroutines and the retained heap.
// Frames are Go frame names (pkg.(*T).Method), root first.

// op is one leaf path under a route's handler frame.
type op struct {
	path       string  // frames below the handler (";"-separated); a key of blocks expands to its frames
	cpu, alloc float64 // relative weights within the route (old version)
	newX       float64 // multiplier in the current version (0 = 1)
	onlyNew    bool    // only in the current version
	onlyOld    bool    // only in the previous version
}

// route is an HTTP route or a consumer process.
type route struct {
	route, method string
	share         float64 // share of the service's request CPU / allocations
	handler       string  // frames from the server loop to the handler (root first)
	ops           []op
}

// heap is a retained-heap stack (inuse_space, no route).
type heap struct {
	path    string
	mib     float64
	newX    float64
	onlyNew bool
}

type serviceSpec struct {
	name, prevVersion string
	cores, allocMiB   float64 // per pod at traffic factor 1: CPU cores, allocated MiB per minute
	routes            []route
	heap              []heap
}

// building blocks (leaf side of common libraries)
var blocks = map[string]string{
	"json.enc":       "encoding/json.Marshal;encoding/json.(*encodeState).marshal;encoding/json.(*encodeState).reflectValue;encoding/json.structEncoder.encode;encoding/json.(*encodeState).string",
	"json.enc.slice": "encoding/json.Marshal;encoding/json.(*encodeState).marshal;encoding/json.(*encodeState).reflectValue;encoding/json.sliceEncoder.encode;encoding/json.arrayEncoder.encode;encoding/json.structEncoder.encode",
	"json.dec":       "encoding/json.(*Decoder).Decode;encoding/json.(*decodeState).unmarshal;encoding/json.(*decodeState).object;encoding/json.(*decodeState).value",
	"sql.query":      "database/sql.(*DB).QueryContext;database/sql.(*DB).queryDC;github.com/jackc/pgx/v5/stdlib.(*Conn).QueryContext;github.com/jackc/pgx/v5/pgconn.(*PgConn).ExecParams;github.com/jackc/pgx/v5/pgconn.(*PgConn).receiveMessage;github.com/jackc/pgx/v5/pgproto3.(*Frontend).Receive",
	"sql.exec":       "database/sql.(*DB).ExecContext;database/sql.(*DB).execDC;github.com/jackc/pgx/v5/stdlib.(*Conn).ExecContext;github.com/jackc/pgx/v5/pgconn.(*PgConn).Exec;internal/poll.(*FD).Write;syscall.write;syscall.Syscall",
	"sql.scan":       "database/sql.(*Rows).Next;database/sql.(*Rows).nextLocked;github.com/jackc/pgx/v5/stdlib.(*Rows).Next;github.com/jackc/pgx/v5/pgtype.(*Map).Scan;runtime.mallocgc",
	"pgx.query":      "github.com/jackc/pgx/v5/pgxpool.(*Pool).Query;github.com/jackc/pgx/v5.(*Conn).Query;github.com/jackc/pgx/v5/pgconn.(*PgConn).ExecParams;github.com/jackc/pgx/v5/pgconn.(*PgConn).receiveMessage;github.com/jackc/pgx/v5/pgproto3.(*Frontend).Receive",
	"pgx.exec":       "github.com/jackc/pgx/v5/pgxpool.(*Pool).Exec;github.com/jackc/pgx/v5.(*Conn).Exec;github.com/jackc/pgx/v5/pgconn.(*PgConn).Exec;internal/poll.(*FD).Write;syscall.write;syscall.Syscall",
	"pgx.scan":       "github.com/jackc/pgx/v5.(*baseRows).Next;github.com/jackc/pgx/v5.(*baseRows).Scan;github.com/jackc/pgx/v5/pgtype.scanPlanBinaryInt8ToInt64.Scan",
	"redis":          "github.com/redis/go-redis/v9.(*baseClient).process;github.com/redis/go-redis/v9.(*baseClient)._process;github.com/redis/go-redis/v9/internal/pool.(*Conn).WithReader;github.com/redis/go-redis/v9/internal/proto.(*Reader).ReadReply",
	"http.client":    "net/http.(*Client).Do;net/http.(*Client).send;net/http.(*Transport).roundTrip;net/http.(*persistConn).roundTrip;runtime.selectgo",
	"tls":            "net/http.(*Client).Do;net/http.(*Transport).roundTrip;net/http.(*persistConn).roundTrip;crypto/tls.(*Conn).Write;crypto/aes.(*gcmAsm).Seal;crypto/aes.gcmAesEnc",
	"kafka":          "github.com/twmb/franz-go/pkg/kgo.(*Client).Produce;github.com/twmb/franz-go/pkg/kgo.(*Client).producerID;github.com/twmb/franz-go/pkg/kgo.(*recBuf).bufferRecord;runtime.mallocgc",
	"amqp":           "github.com/rabbitmq/amqp091-go.(*Channel).PublishWithContext;github.com/rabbitmq/amqp091-go.(*Channel).sendOpen;github.com/rabbitmq/amqp091-go.(*writer).WriteFrame;bufio.(*Writer).Flush",
	"otel":           "go.opentelemetry.io/otel/sdk/trace.(*recordingSpan).End;go.opentelemetry.io/otel/sdk/trace.(*batchSpanProcessor).OnEnd;go.opentelemetry.io/otel/sdk/trace.(*batchSpanProcessor).enqueueDrop",
	"slog":           "log/slog.(*Logger).log;log/slog.(*JSONHandler).Handle;log/slog.(*handleState).appendNonBuiltIns;strconv.AppendQuote",
	"mysql":          "database/sql.(*DB).ExecContext;database/sql.(*DB).execDC;github.com/go-sql-driver/mysql.(*mysqlConn).Exec;github.com/go-sql-driver/mysql.(*mysqlConn).interpolateParams;runtime.mallocgc",
	"bcrypt":         "golang.org/x/crypto/bcrypt.CompareHashAndPassword;golang.org/x/crypto/bcrypt.bcrypt;golang.org/x/crypto/bcrypt.expensiveBlowfishSetup;golang.org/x/crypto/blowfish.ExpandKey;golang.org/x/crypto/blowfish.encryptBlock",
	"jwt":            "github.com/golang-jwt/jwt/v5.(*Token).SignedString;github.com/golang-jwt/jwt/v5.(*SigningMethodECDSA).Sign;crypto/ecdsa.SignASN1;crypto/internal/fips140/nistec.(*P256Point).ScalarBaseMult",
	"gzip":           "compress/gzip.(*Writer).Write;compress/flate.(*compressor).write;compress/flate.(*compressor).deflate;compress/flate.(*compressor).findMatch",
	"malloc":         "runtime.mallocgc;runtime.(*mcache).nextFree;runtime.(*mcache).refill",
}

const httpPrefix = "runtime.goexit;net/http.(*conn).serve;net/http.serverHandler.ServeHTTP;github.com/tasktrooper/telemetry-go/integrations/nethttp.Handler.func1;net/http.(*ServeMux).ServeHTTP;net/http.HandlerFunc.ServeHTTP;"

func h(svc, fn string) string {
	return httpPrefix + "github.com/shop/" + svc + "/internal/api.(*Server)." + fn
}

// background goroutines, common to every service (share of CPU / allocations not attributed to a route)
var background = []op{
	{path: "runtime.gcBgMarkWorker;runtime.systemstack;runtime.gcDrain;runtime.scanobject;runtime.findObject", cpu: 30, alloc: 0},
	{path: "runtime.gcBgMarkWorker;runtime.systemstack;runtime.gcDrain;runtime.markroot;runtime.scanstack", cpu: 8, alloc: 0},
	{path: "runtime.bgsweep;runtime.sweepone;runtime.(*sweepLocked).sweep", cpu: 5, alloc: 0},
	{path: "runtime.mcall;runtime.park_m;runtime.schedule;runtime.findRunnable;runtime.netpoll;runtime.epollwait", cpu: 14, alloc: 0},
	{path: "runtime.goexit;go.opentelemetry.io/otel/sdk/trace.(*batchSpanProcessor).processQueue;go.opentelemetry.io/otel/sdk/trace.(*batchSpanProcessor).exportSpans;go.opentelemetry.io/otel/exporters/otlp/otlptrace.(*Exporter).ExportSpans;go.opentelemetry.io/otel/exporters/otlp/otlptrace/internal/tracetransform.Spans", cpu: 9, alloc: 30},
	{path: "runtime.goexit;go.opentelemetry.io/otel/sdk/trace.(*batchSpanProcessor).processQueue;go.opentelemetry.io/otel/sdk/trace.(*batchSpanProcessor).exportSpans;go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp.(*client).UploadTraces;google.golang.org/protobuf/proto.MarshalOptions.marshal", cpu: 6, alloc: 22},
	{path: "runtime.goexit;go.opentelemetry.io/otel/sdk/metric.(*PeriodicReader).run;go.opentelemetry.io/otel/sdk/metric.(*PeriodicReader).collectAndExport;go.opentelemetry.io/otel/sdk/metric.(*pipeline).produce", cpu: 3, alloc: 8},
	{path: "runtime.goexit;github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck;github.com/jackc/pgx/v5/pgxpool.(*Pool).checkHealth;github.com/jackc/puddle/v2.(*Pool).AcquireAllIdle", cpu: 1, alloc: 1},
	{path: "runtime.goexit;github.com/tasktrooper/telemetry-go.(*profiler).loop;runtime/pprof.(*profileBuilder).build;compress/gzip.(*Writer).Write", cpu: 1.5, alloc: 4},
}

// heapCommon is the retained heap every service has.
var heapCommon = []heap{
	{path: "runtime.goexit;net/http.(*conn).serve;net/http.newBufioReader;bufio.NewReaderSize", mib: 4},
	{path: "runtime.goexit;net/http.(*conn).serve;net/http.newBufioWriterSize;bufio.NewWriterSize", mib: 3},
	{path: "runtime.goexit;go.opentelemetry.io/otel/sdk/trace.(*batchSpanProcessor).processQueue;go.opentelemetry.io/otel/sdk/trace.(*batchSpanProcessor).OnEnd;runtime.growslice", mib: 6},
	{path: "main.main;github.com/tasktrooper/telemetry-go.Start;go.opentelemetry.io/otel/sdk/metric.NewMeterProvider;go.opentelemetry.io/otel/sdk/metric.newPipelines;runtime.makemap", mib: 2},
	{path: "runtime.main;runtime.doInit1;regexp.MustCompile;regexp.compile;regexp/syntax.Compile", mib: 0.6},
}

func specs() []serviceSpec {
	o := "orders-api"
	c := "catalog-api"
	return []serviceSpec{
		{name: o, prevVersion: "v2.40.3", cores: 0.18, allocMiB: 210,
			routes: []route{
				{route: "/v1/orders", method: "POST", share: 0.32, handler: h(o, "createOrder"), ops: []op{
					{path: "json.dec", cpu: 8, alloc: 10},
					{path: "github.com/shop/orders-api/internal/pricing.ApplyDiscounts;github.com/shop/orders-api/internal/pricing.(*RuleSet).Match;regexp.(*Regexp).MatchString;regexp.(*Regexp).doExecute;regexp.(*machine).match", cpu: 4, alloc: 1, newX: 3.2},
					{path: "github.com/shop/orders-api/internal/pricing.ApplyDiscounts;github.com/shop/orders-api/internal/pricing.(*RuleSet).Match;github.com/shop/orders-api/internal/pricing.compileRule;regexp.Compile;regexp.compile;regexp/syntax.Parse", cpu: 15, alloc: 22, onlyNew: true},
					{path: "github.com/shop/orders-api/internal/pricing.ApplyDiscounts;github.com/shop/orders-api/internal/pricing.(*RuleSet).Match;github.com/shop/orders-api/internal/pricing.compileRule;regexp.Compile;regexp.compile;regexp/syntax.Compile;runtime.mallocgc", cpu: 6, alloc: 14, onlyNew: true},
					{path: "github.com/shop/orders-api/internal/pricing.ApplyDiscounts;github.com/shop/orders-api/internal/pricing.roundMoney;math/big.(*Float).SetString;math/big.(*Float).scan", cpu: 3, alloc: 2},
					{path: "github.com/shop/orders-api/internal/store.(*Orders).Insert;sql.exec", cpu: 9, alloc: 4},
					{path: "github.com/shop/orders-api/internal/store.(*Orders).InsertItems;sql.exec", cpu: 6, alloc: 3},
					{path: "github.com/shop/orders-api/internal/inventory.(*Client).Reserve;http.client", cpu: 6, alloc: 5},
					{path: "github.com/shop/orders-api/internal/events.(*Publisher).OrderCreated;kafka", cpu: 5, alloc: 6},
					{path: "github.com/shop/orders-api/internal/events.(*Publisher).OrderCreated;json.enc", cpu: 4, alloc: 7},
					{path: "redis", cpu: 3, alloc: 1},
					{path: "json.enc", cpu: 4, alloc: 6},
					{path: "otel", cpu: 3, alloc: 4},
					{path: "slog", cpu: 2, alloc: 3},
				}},
				{route: "/v1/orders/:id", method: "GET", share: 0.18, handler: h(o, "getOrder"), ops: []op{
					{path: "redis", cpu: 6, alloc: 2},
					{path: "github.com/shop/orders-api/internal/store.(*Orders).Get;sql.query", cpu: 12, alloc: 5},
					{path: "github.com/shop/orders-api/internal/store.(*Orders).Get;sql.scan", cpu: 5, alloc: 8},
					{path: "json.enc", cpu: 10, alloc: 12},
					{path: "otel", cpu: 2, alloc: 3},
				}},
				{route: "/v1/orders", method: "GET", share: 0.2, handler: h(o, "listOrders"), ops: []op{
					{path: "github.com/shop/orders-api/internal/store.(*Orders).List;sql.query", cpu: 14, alloc: 6},
					{path: "github.com/shop/orders-api/internal/store.(*Orders).List;sql.scan", cpu: 12, alloc: 20},
					{path: "json.enc.slice", cpu: 18, alloc: 26},
					{path: "github.com/shop/orders-api/internal/api.paginate;strconv.FormatInt", cpu: 1, alloc: 1},
					{path: "otel", cpu: 2, alloc: 3},
				}},
				{route: "/v1/orders/:id/cancel", method: "POST", share: 0.04, handler: h(o, "cancelOrder"), ops: []op{
					{path: "github.com/shop/orders-api/internal/store.(*Orders).UpdateStatus;sql.exec", cpu: 8, alloc: 3},
					{path: "github.com/shop/orders-api/internal/events.(*Publisher).StatusChanged;kafka", cpu: 4, alloc: 4},
					{path: "github.com/shop/orders-api/internal/notify.(*Mailer).Enqueue;amqp", cpu: 3, alloc: 3},
				}},
				{route: "/v1/cart", method: "GET", share: 0.14, handler: h(o, "getCart"), ops: []op{
					{path: "redis", cpu: 10, alloc: 4},
					{path: "github.com/shop/orders-api/internal/pricing.ApplyDiscounts;github.com/shop/orders-api/internal/pricing.(*RuleSet).Match;regexp.(*Regexp).MatchString;regexp.(*Regexp).doExecute;regexp.(*machine).match", cpu: 3, alloc: 1, newX: 3.2},
					{path: "github.com/shop/orders-api/internal/pricing.ApplyDiscounts;github.com/shop/orders-api/internal/pricing.(*RuleSet).Match;github.com/shop/orders-api/internal/pricing.compileRule;regexp.Compile;regexp.compile;regexp/syntax.Parse", cpu: 9, alloc: 14, onlyNew: true},
					{path: "json.enc", cpu: 8, alloc: 10},
				}},
				{route: "/v1/cart/items/:itemId", method: "PUT", share: 0.08, handler: h(o, "putCartItem"), ops: []op{
					{path: "json.dec", cpu: 5, alloc: 6},
					{path: "redis", cpu: 8, alloc: 3},
					{path: "json.enc", cpu: 4, alloc: 5},
				}},
				{route: "/healthz", method: "GET", share: 0.04, handler: h(o, "healthz"), ops: []op{
					{path: "github.com/shop/orders-api/internal/store.(*DB).Ping;database/sql.(*DB).PingContext;github.com/jackc/pgx/v5/pgconn.(*PgConn).Ping", cpu: 3, alloc: 1},
					{path: "net/http.(*response).Write;bufio.(*Writer).Write", cpu: 1, alloc: 1},
				}},
			},
			heap: []heap{
				{path: "runtime.goexit;github.com/shop/orders-api/internal/pricing.(*RuleSet).refreshLoop;github.com/shop/orders-api/internal/pricing.(*RuleSet).load;runtime.makemap", mib: 9},
				{path: "runtime.goexit;github.com/shop/orders-api/internal/api.(*Server).createOrder;github.com/shop/orders-api/internal/pricing.ApplyDiscounts;github.com/shop/orders-api/internal/pricing.compileRule;regexp.Compile;regexp/syntax.Compile", mib: 14, onlyNew: true},
				{path: "runtime.goexit;database/sql.(*DB).connectionOpener;database/sql.(*DB).openNewConnection;github.com/jackc/pgx/v5/pgconn.connectOne;github.com/jackc/pgx/v5/pgproto3.NewFrontend", mib: 5},
				{path: "runtime.goexit;github.com/twmb/franz-go/pkg/kgo.(*sink).drain;github.com/twmb/franz-go/pkg/kgo.(*recBuf).bufferRecord;runtime.growslice", mib: 7},
				{path: "runtime.goexit;github.com/redis/go-redis/v9/internal/pool.(*ConnPool).dialConn;github.com/redis/go-redis/v9/internal/proto.NewReader;bufio.NewReaderSize", mib: 2},
			}},
		{name: "payments-api", prevVersion: "v1.18.2", cores: 0.09, allocMiB: 90,
			routes: []route{
				{route: "/v1/charges", method: "POST", share: 0.55, handler: h("payments-api", "createCharge"), ops: []op{
					{path: "json.dec", cpu: 6, alloc: 8},
					{path: "github.com/shop/payments-api/internal/store.(*Charges).Insert;pgx.query", cpu: 10, alloc: 4},
					{path: "github.com/shop/payments-api/internal/store.(*Charges).UpdateStatus;pgx.exec", cpu: 9, alloc: 3},
					{path: "github.com/shop/payments-api/internal/iyzico.(*Client).Authorize;tls", cpu: 14, alloc: 6},
					{path: "github.com/shop/payments-api/internal/iyzico.(*Client).Authorize;github.com/shop/payments-api/internal/iyzico.sign;crypto/hmac.(*hmac).Sum;crypto/sha256.(*Digest).checkSum", cpu: 5, alloc: 1},
					{path: "github.com/shop/payments-api/internal/events.(*Publisher).ChargeCaptured;kafka", cpu: 5, alloc: 6},
					{path: "github.com/shop/payments-api/internal/notify.(*Mailer).Receipt;amqp", cpu: 3, alloc: 3},
					{path: "redis", cpu: 4, alloc: 2},
					{path: "otel", cpu: 3, alloc: 4},
				}},
				{route: "/v1/refunds", method: "POST", share: 0.08, handler: h("payments-api", "createRefund"), ops: []op{
					{path: "github.com/shop/payments-api/internal/iyzico.(*Client).Refund;tls", cpu: 10, alloc: 5},
					{path: "github.com/shop/payments-api/internal/store.(*Refunds).Insert;pgx.exec", cpu: 8, alloc: 3},
				}},
				{route: "/v1/charges/:id", method: "GET", share: 0.2, handler: h("payments-api", "getCharge"), ops: []op{
					{path: "redis", cpu: 8, alloc: 3},
					{path: "github.com/shop/payments-api/internal/store.(*Charges).Get;pgx.query", cpu: 8, alloc: 4},
					{path: "json.enc", cpu: 6, alloc: 7},
				}},
				{route: "/v1/webhooks/stripe", method: "POST", share: 0.17, handler: h("payments-api", "stripeWebhook"), ops: []op{
					{path: "github.com/stripe/stripe-go/v81/webhook.ConstructEvent;github.com/stripe/stripe-go/v81/webhook.ValidatePayload;crypto/hmac.Equal", cpu: 4, alloc: 2},
					{path: "json.dec", cpu: 9, alloc: 12},
					{path: "github.com/shop/payments-api/internal/stripe.(*Client).Event;tls", cpu: 8, alloc: 5},
					{path: "github.com/shop/payments-api/internal/store.(*Charges).Settle;pgx.exec", cpu: 6, alloc: 3},
				}},
			},
			heap: []heap{
				{path: "runtime.goexit;crypto/tls.(*Conn).Handshake;crypto/tls.(*Conn).clientHandshake;crypto/x509.(*Certificate).Verify", mib: 3},
				{path: "runtime.goexit;github.com/jackc/pgx/v5/pgxpool.(*Pool).createIdleResources;github.com/jackc/pgx/v5.connect;github.com/jackc/pgx/v5/pgproto3.NewFrontend", mib: 4},
			}},
		{name: c, prevVersion: "v3.1.4", cores: 0.11, allocMiB: 160,
			routes: []route{
				{route: "/v1/items", method: "GET", share: 0.3, handler: h(c, "listItems"), ops: []op{
					{path: "redis", cpu: 10, alloc: 6},
					{path: "github.com/shop/catalog-api/internal/store.(*Items).ByCategory;pgx.query", cpu: 8, alloc: 4},
					{path: "github.com/shop/catalog-api/internal/store.(*Items).ByCategory;pgx.scan", cpu: 6, alloc: 12},
					{path: "json.enc.slice", cpu: 14, alloc: 22},
				}},
				{route: "/v1/items/:id", method: "GET", share: 0.3, handler: h(c, "getItem"), ops: []op{
					{path: "github.com/redis/go-redis/v9.(*Client).MGet;redis", cpu: 8, alloc: 4},
					{path: "github.com/shop/catalog-api/internal/store.(*Items).Get;pgx.query", cpu: 6, alloc: 3},
					{path: "json.enc", cpu: 6, alloc: 7},
				}},
				{route: "/v1/search", method: "GET", share: 0.32, handler: h(c, "search"), ops: []op{
					{path: "github.com/shop/catalog-api/internal/search.Tokenize;strings.Fields", cpu: 3, alloc: 4},
					{path: "github.com/shop/catalog-api/internal/search.Tokenize;strings.ToLower;unicode.ToLower", cpu: 2, alloc: 3, newX: 2.2},
					{path: "github.com/shop/catalog-api/internal/search.(*Embedder).Embed;github.com/tasktrooper/telemetry-go/llm.(*transport).RoundTrip;tls", cpu: 4, alloc: 3, onlyNew: true},
					{path: "github.com/shop/catalog-api/internal/search.(*Embedder).Embed;json.dec", cpu: 6, alloc: 14, onlyNew: true},
					{path: "github.com/shop/catalog-api/internal/search.Rerank;github.com/shop/catalog-api/internal/search.cosine", cpu: 9, alloc: 1, onlyNew: true},
					{path: "github.com/shop/catalog-api/internal/store.(*Items).Search;pgx.query", cpu: 12, alloc: 5},
					{path: "github.com/shop/catalog-api/internal/store.(*Items).Search;pgx.scan", cpu: 6, alloc: 10},
					{path: "json.enc.slice", cpu: 8, alloc: 12},
				}},
				{route: "/v1/items", method: "POST", share: 0.08, handler: h(c, "createItem"), ops: []op{
					{path: "json.dec", cpu: 6, alloc: 8},
					{path: "github.com/shop/catalog-api/internal/store.(*Items).Insert;pgx.exec", cpu: 6, alloc: 3},
					{path: "github.com/shop/catalog-api/internal/events.(*Publisher).ItemCreated;kafka", cpu: 4, alloc: 4},
				}},
			},
			heap: []heap{
				{path: "runtime.goexit;github.com/shop/catalog-api/internal/search.(*Index).refresh;github.com/shop/catalog-api/internal/search.(*Index).build;runtime.makemap", mib: 22},
				{path: "runtime.goexit;github.com/shop/catalog-api/internal/search.(*Embedder).warm;github.com/shop/catalog-api/internal/search.newVectorCache;runtime.makeslice", mib: 12, onlyNew: true},
			}},
		{name: "auth-api", prevVersion: "v5.0.1", cores: 0.06, allocMiB: 70,
			routes: []route{
				{route: "/v1/auth/refresh", method: "POST", share: 0.45, handler: h("auth-api", "refresh"), ops: []op{
					{path: "github.com/shop/auth-api/internal/store.(*Tokens).Lookup;pgx.query", cpu: 8, alloc: 3},
					{path: "github.com/shop/auth-api/internal/token.Issue;jwt", cpu: 16, alloc: 4},
					{path: "github.com/shop/auth-api/internal/session.(*Store).Put;redis", cpu: 6, alloc: 6},
					{path: "github.com/shop/auth-api/internal/events.(*Publisher).Refreshed;kafka", cpu: 3, alloc: 3},
				}},
				{route: "/v1/auth/login", method: "POST", share: 0.25, handler: h("auth-api", "login"), ops: []op{
					{path: "github.com/shop/auth-api/internal/password.Verify;bcrypt", cpu: 60, alloc: 2},
					{path: "github.com/shop/auth-api/internal/store.(*Users).ByEmail;pgx.query", cpu: 5, alloc: 3},
					{path: "github.com/shop/auth-api/internal/token.Issue;jwt", cpu: 10, alloc: 3},
				}},
				{route: "/v1/auth/me", method: "GET", share: 0.28, handler: h("auth-api", "me"), ops: []op{
					{path: "github.com/shop/auth-api/internal/token.Verify;github.com/golang-jwt/jwt/v5.(*Parser).ParseWithClaims;crypto/ecdsa.VerifyASN1", cpu: 9, alloc: 3},
					{path: "redis", cpu: 6, alloc: 2},
				}},
				{route: "/v1/auth/logout", method: "POST", share: 0.02, handler: h("auth-api", "logout"), ops: []op{{path: "redis", cpu: 4, alloc: 1}}},
			},
			heap: []heap{{path: "runtime.goexit;github.com/shop/auth-api/internal/token.(*KeySet).refresh;crypto/x509.ParsePKIXPublicKey", mib: 1}}},
		{name: "storefront-bff", prevVersion: "v1.9.3", cores: 0.15, allocMiB: 260,
			routes: []route{
				{route: "/v1/home", method: "GET", share: 0.4, handler: h("storefront-bff", "home"), ops: []op{
					{path: "golang.org/x/sync/errgroup.(*Group).Wait;runtime.semacquire", cpu: 1, alloc: 0.5},
					{path: "github.com/shop/storefront-bff/internal/compose.(*Home).Build;github.com/shop/storefront-bff/internal/clients.(*Catalog).Items;http.client", cpu: 8, alloc: 6},
					{path: "github.com/shop/storefront-bff/internal/compose.(*Home).Build;github.com/shop/storefront-bff/internal/clients.(*Reco).For;http.client", cpu: 8, alloc: 6},
					{path: "github.com/shop/storefront-bff/internal/compose.(*Home).Build;json.dec", cpu: 16, alloc: 24},
					{path: "github.com/shop/storefront-bff/internal/compose.(*Home).Build;github.com/shop/storefront-bff/internal/compose.mergeRails;runtime.growslice", cpu: 5, alloc: 10},
					{path: "redis", cpu: 6, alloc: 3},
					{path: "json.enc.slice", cpu: 20, alloc: 28},
					{path: "github.com/shop/storefront-bff/internal/api.(*Server).writeCompressed;gzip", cpu: 12, alloc: 3},
				}},
				{route: "/v1/product/:slug", method: "GET", share: 0.38, handler: h("storefront-bff", "product"), ops: []op{
					{path: "github.com/shop/storefront-bff/internal/compose.(*Product).Build;github.com/shop/storefront-bff/internal/clients.(*Catalog).Item;http.client", cpu: 10, alloc: 7},
					{path: "github.com/shop/storefront-bff/internal/compose.(*Product).Build;github.com/shop/storefront-bff/internal/clients.(*Reco).For;http.client", cpu: 7, alloc: 5},
					{path: "github.com/shop/storefront-bff/internal/compose.(*Product).Build;json.dec", cpu: 12, alloc: 18},
					{path: "json.enc", cpu: 14, alloc: 18},
					{path: "github.com/shop/storefront-bff/internal/api.(*Server).writeCompressed;gzip", cpu: 9, alloc: 2},
				}},
				{route: "/v1/search", method: "GET", share: 0.14, handler: h("storefront-bff", "search"), ops: []op{
					{path: "github.com/shop/storefront-bff/internal/clients.(*Catalog).Search;http.client", cpu: 8, alloc: 5},
					{path: "json.dec", cpu: 8, alloc: 12},
					{path: "json.enc.slice", cpu: 8, alloc: 10},
				}},
				{route: "/v1/assistant/messages", method: "POST", share: 0.08, handler: h("storefront-bff", "assistantMessage"), ops: []op{
					{path: "github.com/shop/storefront-bff/internal/assistant.(*Agent).Turn;github.com/anthropics/anthropic-sdk-go/packages/ssestream.(*Stream).Next;bufio.(*Scanner).Scan", cpu: 9, alloc: 12},
					{path: "github.com/shop/storefront-bff/internal/assistant.(*Agent).Turn;github.com/anthropics/anthropic-sdk-go.(*MessageService).NewStreaming;tls", cpu: 6, alloc: 4},
					{path: "github.com/shop/storefront-bff/internal/assistant.(*Agent).Turn;github.com/anthropics/anthropic-sdk-go.(*Message).Accumulate;json.dec", cpu: 8, alloc: 14},
					{path: "github.com/shop/storefront-bff/internal/assistant.(*Agent).runTool;github.com/shop/storefront-bff/internal/clients.(*Orders).Get;http.client", cpu: 3, alloc: 2},
				}},
			},
			heap: []heap{
				{path: "runtime.goexit;github.com/shop/storefront-bff/internal/compose.(*LayoutCache).refresh;json.dec", mib: 16},
				{path: "runtime.goexit;net/http.(*Transport).dialConnFor;net/http.(*Transport).dialConn;bufio.NewReaderSize", mib: 6},
			}},
		{name: "recommendation-api", prevVersion: "v0.13.2", cores: 0.2, allocMiB: 140,
			routes: []route{
				{route: "/v1/recommendations", method: "GET", share: 0.85, handler: h("recommendation-api", "recommendations"), ops: []op{
					{path: "github.com/shop/recommendation-api/internal/store.(*Reco).TopForUser;pgx.query", cpu: 14, alloc: 5},
					{path: "github.com/shop/recommendation-api/internal/store.(*Reco).TopForUser;pgx.scan", cpu: 8, alloc: 12},
					{path: "github.com/shop/recommendation-api/internal/rank.Blend;sort.Slice;sort.pdqsort_func;sort.insertionSort_func", cpu: 18, alloc: 6, onlyOld: true},
					{path: "github.com/shop/recommendation-api/internal/rank.Blend;slices.SortFunc;slices.pdqsortCmpFunc", cpu: 7, alloc: 1, onlyNew: true},
					{path: "github.com/shop/recommendation-api/internal/rank.Blend;github.com/shop/recommendation-api/internal/rank.score;math.Exp", cpu: 10, alloc: 0.5},
					{path: "github.com/shop/recommendation-api/internal/rank.Dedupe;runtime.mapassign_fast64", cpu: 6, alloc: 8},
					{path: "redis", cpu: 5, alloc: 2},
					{path: "json.enc.slice", cpu: 8, alloc: 12},
				}},
				{route: "/v1/events", method: "POST", share: 0.15, handler: h("recommendation-api", "events"), ops: []op{
					{path: "json.dec", cpu: 6, alloc: 8},
					{path: "redis", cpu: 5, alloc: 2},
					{path: "kafka", cpu: 6, alloc: 7},
				}},
			},
			heap: []heap{
				{path: "runtime.goexit;github.com/shop/recommendation-api/internal/model.(*Embeddings).Load;github.com/shop/recommendation-api/internal/model.readMatrix;runtime.makeslice", mib: 48},
				{path: "runtime.goexit;github.com/shop/recommendation-api/internal/rank.(*SegmentCache).refresh;runtime.makemap", mib: 11},
			}},
		{name: "order-events-consumer", prevVersion: "v2.40.3", cores: 0.06, allocMiB: 80,
			routes: consumerRoutes(),
			heap: []heap{
				{path: "runtime.goexit;github.com/twmb/franz-go/pkg/kgo.(*source).loopFetch;github.com/twmb/franz-go/pkg/kgo.(*source).fetch;github.com/twmb/franz-go/pkg/kmsg.(*FetchResponse).ReadFrom;runtime.makeslice", mib: 18},
				{path: "runtime.goexit;github.com/twmb/franz-go/pkg/kgo.(*source).fetch;github.com/klauspost/compress/zstd.(*Decoder).DecodeAll;runtime.makeslice", mib: 7},
			}},
		{name: "email-worker", prevVersion: "v0.9.11", cores: 0.04, allocMiB: 45,
			routes: emailRoutes(),
			heap: []heap{
				{path: "runtime.goexit;github.com/shop/email-worker/internal/templates.(*Registry).Load;html/template.(*Template).Parse;text/template/parse.Parse", mib: 6},
				{path: "runtime.goexit;github.com/rabbitmq/amqp091-go.(*Connection).reader;bufio.NewReaderSize", mib: 1},
			}},
	}
}

const consumerPrefix = "runtime.goexit;github.com/shop/order-events/internal/consumer.(*Loop).Run.func1;github.com/tasktrooper/telemetry-go/integrations/franzgo.(*Tracer).ProcessRecord;github.com/tasktrooper/telemetry-go.ProcessMessage;"

func consumerRoutes() []route {
	p := func(fn string) string { return consumerPrefix + "github.com/shop/order-events/internal/handlers." + fn }
	return []route{
		{route: "order-created-handler", method: "CONSUME", share: 0.45, handler: p("(*OrderCreated).Handle"), ops: []op{
			{path: "json.dec", cpu: 8, alloc: 10},
			{path: "github.com/shop/order-events/internal/fulfilment.(*Planner).Plan;github.com/shop/order-events/internal/fulfilment.(*Planner).pickWarehouse;sort.Slice", cpu: 9, alloc: 4},
			{path: "github.com/shop/order-events/internal/store.(*Shipments).Insert;sql.exec", cpu: 8, alloc: 3},
			{path: "github.com/shop/order-events/internal/store.(*Shipments).LockMerchant;sql.query", cpu: 5, alloc: 2},
		}},
		{route: "order-search-indexer", method: "CONSUME", share: 0.3, handler: p("(*SearchIndexer).Handle"), ops: []op{
			{path: "json.dec", cpu: 8, alloc: 10},
			{path: "github.com/shop/order-events/internal/search.(*Doc).Build;strings.(*Builder).WriteString", cpu: 6, alloc: 10},
			{path: "github.com/shop/order-events/internal/search.(*Client).Index;http.client", cpu: 8, alloc: 6},
		}},
		{route: "order-status-projector", method: "CONSUME", share: 0.2, handler: p("(*StatusProjector).Handle"), ops: []op{
			{path: "json.dec", cpu: 6, alloc: 7},
			{path: "github.com/shop/order-events/internal/store.(*Projection).Upsert;sql.exec", cpu: 9, alloc: 3},
		}},
		{route: "dlq-replayer", method: "CONSUME", share: 0.05, handler: p("(*DLQReplayer).Handle"), ops: []op{
			{path: "github.com/shop/order-events/internal/dlq.(*Replayer).Republish;kafka", cpu: 6, alloc: 6},
		}},
	}
}

const amqpPrefix = "runtime.goexit;github.com/shop/email-worker/internal/worker.(*Pool).run.func1;github.com/tasktrooper/telemetry-go/integrations/amqp091.ProcessDelivery;github.com/tasktrooper/telemetry-go.ProcessMessage;"

func emailRoutes() []route {
	p := func(fn string) string { return amqpPrefix + "github.com/shop/email-worker/internal/senders." + fn }
	return []route{
		{route: "email-sender", method: "CONSUME", share: 0.55, handler: p("(*Transactional).Send"), ops: []op{
			{path: "github.com/shop/email-worker/internal/templates.(*Registry).Render;html/template.(*Template).Execute;text/template.(*state).walk", cpu: 14, alloc: 18},
			{path: "github.com/shop/email-worker/internal/smtp.(*Client).Send;net/smtp.(*Client).Data;crypto/tls.(*Conn).Write", cpu: 10, alloc: 4},
			{path: "github.com/shop/email-worker/internal/store.(*Log).Insert;mysql", cpu: 6, alloc: 4},
			{path: "amqp", cpu: 3, alloc: 2},
		}},
		{route: "newsletter-sender", method: "CONSUME", share: 0.35, handler: p("(*Newsletter).Send"), ops: []op{
			{path: "github.com/shop/email-worker/internal/templates.(*Registry).Render;html/template.(*Template).Execute;text/template.(*state).walk", cpu: 16, alloc: 22},
			{path: "github.com/shop/email-worker/internal/personalize.(*Subjects).Generate;google.golang.org/genai.(*Models).GenerateContent;tls", cpu: 5, alloc: 4, onlyNew: true},
			{path: "github.com/shop/email-worker/internal/personalize.(*Subjects).Generate;google.golang.org/genai.(*Models).GenerateContent;json.dec", cpu: 4, alloc: 8, onlyNew: true},
			{path: "github.com/shop/email-worker/internal/smtp.(*Client).Send;net/smtp.(*Client).Data;crypto/tls.(*Conn).Write", cpu: 10, alloc: 4},
			{path: "github.com/shop/email-worker/internal/store.(*Unsubscribes).Has;database/sql.(*DB).QueryRowContext;github.com/go-sql-driver/mysql.(*mysqlConn).query", cpu: 4, alloc: 2},
		}},
		{route: "invoice-batch", method: "CONSUME", share: 0.1, handler: "runtime.goexit;github.com/shop/email-worker/internal/cron.(*Scheduler).run;github.com/tasktrooper/telemetry-go.ProcessMessage;github.com/shop/email-worker/internal/invoices.(*Batch).Run", ops: []op{
			{path: "github.com/shop/email-worker/internal/invoices.(*Batch).Run;github.com/go-pdf/fpdf.(*Fpdf).Output;compress/zlib.(*Writer).Write;compress/flate.(*compressor).deflate", cpu: 14, alloc: 10},
			{path: "mysql", cpu: 6, alloc: 3},
		}},
	}
}
