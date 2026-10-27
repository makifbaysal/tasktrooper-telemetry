# Veri modeli

> Uygulanan şema: `deploy/clickhouse/001_init.sql` ve `deploy/postgres/001_init.sql` (tek doğru kaynak). Bu dosya
> tasarım gerekçesini anlatır. Rollup'lar (`red_1m/1h`, `edges_1m` (= deps_1m), `callers_1m`, `consumer_1m`, `errors_1h`,
> `rum_views_1m`, `rum_vitals_1m`, `rum_resources_1m`, `trace_index`, `deploys`) ham `spans`/`rum_events` üzerinde materialized view;
> `edges_1m` join gerektirmez çünkü SDK her span'a `trooper.entry.route` yazar.

## ClickHouse (telemetri)

Tüm tablolar `ReplacingMergeTree` değil `MergeTree` ailesi; ORDER BY tenant ile başlar. TTL tablo bazlı, plan'a göre override edilir.

### `spans`
```sql
CREATE TABLE spans (
  org_id UInt32, project_id UInt32,
  ts DateTime64(6), duration_ns UInt64,
  trace_id FixedString(16), span_id FixedString(8), parent_span_id FixedString(8),
  service LowCardinality(String), env LowCardinality(String), version LowCardinality(String),
  kind Enum8('server'=1,'client'=2,'producer'=3,'consumer'=4,'internal'=5),
  name String, route LowCardinality(String), http_method LowCardinality(String), status_code UInt16,
  is_error UInt8, error_fingerprint UInt64,
  attrs Map(LowCardinality(String), String)
) ENGINE = MergeTree
PARTITION BY toDate(ts)
ORDER BY (org_id, project_id, service, route, ts)
TTL toDateTime(ts) + INTERVAL 15 DAY;
```

### `red_1m` (rollup, ingest-writer üretir)
`org_id, project_id, service, env, route, method, minute, count, errors, duration_hist (AggregateFunction(quantilesTDigest...)), status_2xx/3xx/4xx/5xx`
→ MV ile `red_1h`. Retention: 1m → 30 gün, 1h → 13 ay.

### `consumer_1m`
`service, topic, group, minute, processed, failed, retried, dlq, duration_hist, max_lag`
- UI'daki birim `(service, process=group)`; `topic` ayrı boyut. Bir topic → N process (her biri ayrı group, ayrı lag).

### `logs`
`ts, org_id, project_id, service, env, severity, body, trace_id, span_id, attrs Map, pattern_id UInt64`
- `body` üzerinde `tokenbf_v1` skip index (full-text arama).
- `pattern_id`: Drain algoritması ile ingest'te çıkarılan log şablonu → log pattern alert'leri ve gruplama için.

### `rum_events`
`ts, org_id, project_id, service, env, type, session_id, view_id, view_name, anonymous_id, user_id, country, device_type, browser, os, app_version, duration_ms, vital_name, vital_value, error_fingerprint, attrs Map`
→ MV: `rum_views_1m` (view bazlı görüntülenme, p75 LCP/INP/CLS, hata), `rum_sessions` (AggregatingMergeTree, session özetleri).

### `errors`
Fingerprint bazlı issue tablosu: `first_seen, last_seen, count, sample_event_id, status (open/resolved/ignored)` — Postgres'te status, ClickHouse'ta sayımlar.

### `db_1m` (DB/cache metrikleri; agent, cloud integration ya da direct collector yazar)
`database_id, node, minute, cpu, memory, disk, connections, max_connections, ops, p95_ms, slow, hit_ratio, evictions, repl_lag_ms`
- Top queries: `db_queries_1m(database_id, query_hash, minute, calls, total_ms, p95_ms, rows)`; metin normalize edilir (parametreler `$1`), PII içermez. Çağıran servis/endpoint, span'lerdeki `db.statement` hash'i ile eşlenir.
- Servis ↔ DB eşleşmesi: bağımlılık adı (`postgres · orders-db`) = `databases.name`.

### `broker_lag_1m` (broker collector yazar)
`cluster_id, topic, group, partition, minute, end_offset, committed_offset, lag, consume_rate, produce_rate` — RabbitMQ: `partition = 0`, `lag = messages_ready`, ayrıca `unacked`.
- Kafka: AdminClient ile topic end offset'leri ve group committed offset'leri 15 sn'de bir okunur; mesaj tüketilmez. Group ↔ servis eşleşmesi SDK'nın raporladığı `group.id` (RabbitMQ: consumer tag) ile.
- `consumer_1m.max_lag` SDK'dan gelir; broker yoksa o kullanılır, broker bağlıysa broker değeri esastır.

### Host, container, process, Kubernetes ve serverless (ADR-019/020, `deploy/clickhouse/003_infra_k8s_faas.sql`)
Hepsi `AggregatingMergeTree`, `PARTITION BY toYYYYMM(minute)`, satır = varlık boyutları + dakika. Gauge'lar
`SimpleAggregateFunction(max, Float64)`; raporlanmayan değer `-Inf` (contracts §5, okuyan `isFinite`). Boş `cluster` = K8s dışı.
Yazan: ingest-writer (`trooper.host|container|process|k8s|faas.*` OTLP gauge'ları; metrik → kolon eşlemesi contracts §20/§21).

| Tablo | Boyutlar (ORDER BY) | Değerler | TTL |
|---|---|---|---|
| `hosts_1m` | `cluster, host, host_id, os, arch, node` (`host, minute`) | `cpu, cpu_cores, memory, memory_bytes, disk, disk_read, disk_write, net_in, net_out, load1, uptime_s` | 30 gün |
| `containers_1m` | `cluster, host, namespace, pod, pod_uid, container, container_id, image, workload_kind, workload, service` (`cluster, namespace, pod, container, host, minute`) | `cpu, cpu_limit, memory_bytes, memory_limit, restarts, net_in, net_out, throttled` | 30 gün |
| `processes_1m` | `host, container_id, pid UInt32, exe, command` (`host, pid, exe, minute`) | `cpu, memory_bytes, threads, fds` | 7 gün |
| `k8s_nodes_1m` | `cluster, node` | `ready, allocatable_cpu, allocatable_memory, pods` | 30 gün |
| `k8s_pods_1m` | `cluster, namespace, pod, pod_uid, node, workload_kind, workload, service` (`cluster, namespace, pod, pod_uid, minute`) | `phase`, `reason` (`SimpleAggregateFunction(anyLast, String)`), `ready, restarts, cpu_request, cpu_limit, memory_request, memory_limit` | 30 gün |
| `k8s_workloads_1m` | `cluster, namespace, kind, name` | `desired, ready, available, updated` | 30 gün |
| `faas_1m` | `function_id, provider, region` (`function_id, minute`) | `invocations, errors, throttles, duration_avg, duration_max, duration_p95, duration_p99, concurrency, iterator_age_ms, memory_bytes, execution_units` | 30 gün |
| `gpus_1m` (`004_gpu.sql`, contracts §22) | `cluster, host, gpu_index UInt8, gpu_uuid, model` (`host, gpu_index, gpu_uuid, minute`) | `utilization` (0..1), `memory` (0..1), `memory_bytes`, `temperature_c`, `power_w` | 30 gün |

- `gpus_1m`: trooper-agent host modu `nvidia-smi` gauge'ları (`trooper.host.gpu.*`, attr `gpu.index|uuid|model`); `hosts_1m`
  değişmez. api host satırına son dakikadaki GPU'ları (`gpus`), host detayına GPU başına utilization serisini (`series.gpu`,
  id `gpu<index>`) ekler. Host'undan 3 dk önce susan GPU listelenmez.
- Azure fonksiyon satırları (`serverless_functions.meta.parent` = app slug'ı, App Insights) kendi `faas_1m` satırlarını
  (`function_id` = fonksiyon slug'ı) yazar; app satırı app seviyesi toplamdır. Özet toplamlarında fonksiyon satırları sayılmaz.
- `containers_1m` sıralama anahtarında `host` da var (sözleşmeye ek): K8s dışı aynı adlı container'lar (pod/namespace boş)
  farklı host'larda birleşmesin diye.
- Sorgu kalıbı: önce `GROUP BY <anahtar>, minute` ile `max(...)` (aynı dakikanın parçalı satırları), sonra `isFinite` süzülüp
  aralık toplaması. `restarts` kümülatif sayaçtır: aralıktaki artış = `max − min`. `faas_1m` aralık toplamı = dakikaların `sum`'ı.
- K8s Warning event'leri ayrı tablo değil: `logs` (`service='k8s'`, `attrs['k8s.event.*']`, `attrs['k8s.cluster.name']` —
  writer log satırına resource'taki `k8s.cluster.name|namespace.name|pod.name|node.name`'i kopyalar).
- eBPF (OBI) span'ları normal `spans` yolundan gelir; kaynak `resource_attrs['telemetry.sdk.name'] IN
  ('opentelemetry-ebpf-instrumentation', 'beyla')`.

## PostgreSQL (metadata)

```
orgs(id, name, plan, created_at)
users(id, email, name) · memberships(org_id, user_id, role)
projects(id, org_id, name, slug)
api_keys(id, project_id, kind ingest|api, hash, allowed_origins[], last_used_at)
services(id, project_id, name, type, language, first_seen, last_seen)
endpoints(id, service_id, method, route, source enum(observed|code|both), code_ref jsonb)
monitors(id, project_id, name, kind, query jsonb, condition jsonb, severity, source enum(manual|suggested|template),
         status enum(active|muted|draft), suggestion_id, created_by)
incidents(id, monitor_id, kpi_id null, state enum(firing|acknowledged|resolved), started_at, resolved_at, peak_value,
          root_cause jsonb)            -- RcaResult snapshot: path, root node, confidence, summary
kpis(id, project_id, name, description, owner, target jsonb, sli enum(latency|availability|success_rate),
     good_when jsonb, objective real, window enum(7d|28d), source enum(code|template|manual), created_by)
kpi_alert_rules(id, kpi_id, kind enum(burn_rate|below_target), long_window, short_window, threshold real,
                severity, channels text[])
notification_channels(id, project_id, type, config jsonb)
repositories(id, project_id, provider, full_name, default_branch, last_analyzed_sha, status)
inventory_items(id, repository_id, kind endpoint|consumer|cron|view|dependency, key, source, file, line, meta jsonb)
-- consumer/cron: key = process adı, source = topic/queue ya da schedule
alert_suggestions(id, inventory_item_id, monitor_draft jsonb, rationale text, confidence real, state proposed|accepted|rejected)
databases(id, project_id, name, engine postgres|mysql|redis|mongodb, host, collector agent|cloud|direct,
          credentials_ref, cloud_resource_id, created_at)      -- credentials secret store'da, burada sadece ref
broker_clusters(id, project_id, name, kind kafka|rabbitmq, endpoint, vhost, auth jsonb, credentials_ref, created_at)
loadtest_scenarios(id, project_id, name, spec jsonb, source manual|generated)
loadtest_runs(id, scenario_id, state, started_at, finished_at, summary jsonb, thresholds_passed bool)
```

## Kardinalite kuralları

- `route` her zaman template: `/orders/123` → `/orders/:id`. SDK framework router'ından alır; alamazsa ingest-writer heuristik normalize eder (sayı, uuid, hex, ulid → `:id`).
- Proje başına benzersiz route limiti: 2.000. Aşılırsa yeni route'lar `__other__` altında toplanır ve UI'da uyarı gösterilir.
- `attrs` map'inde key sayısı ≤ 64, değer ≤ 1 KB.
- `user_id` sadece `rum_events` ve span attrs'ta; rollup tablolarında asla.

## Giden trafik & süre dağılımı

Kaynak: server span'ın altındaki `client` / `producer` span'ları (DB, cache, HTTP, queue). Kategori OTel attribute'larından çıkarılır:
`db.system` → `db` (redis/memcached → `cache`), `http.request.method` + kind=client → `http`, `messaging.system` → `queue`. Geri kalan süre = `app` (kendi kod).

### `deps_1m` (rollup, ingest-writer üretir)
`org_id, project_id, service, env, route, method, dependency, dep_kind, minute, calls, errors, duration_sum, duration_hist`
- Giden trafik = `calls`, endpoint başına fan-out = `calls / red_1m.count`.
- Endpoint süre dağılımı = `duration_sum / red_1m.count` (kategori bazlı). `app = avg_duration − Σ dependency süreleri` (paralel çağrılarda kritik yol ile düzeltilir, v2).
- `dependency` adı normalize: `postgres · <db.name>`, `HTTP · <peer.service | server.address>`, `kafka · <topic>`.

## Gelen trafik kaynağı (callers)

Her server span için caller şu sırayla çözülür (ilk eşleşen kazanır):
1. **Trace context:** parent span başka bir serviste → `service` (ör. orders-api). RUM SDK'dan geliyorsa → `web` / `mobile` (shop-web, shop-mobile).
2. **API key:** isteği taşıyan proje API key'inin adı → `partner` (ör. "acme-erp").
3. **User-agent sınıflandırma:** bilinen webhook göndericileri (Stripe, GitHub, Shopify...) → `webhook`; `kube-probe`, ELB health checker → `probe`.
4. Hiçbiri → `unknown` ("Uninstrumented"). UI bunu uyarı olarak gösterir: caller'a SDK eklenmeli.

### `callers_1m` (rollup)
`org_id, project_id, service, env, route, method, caller, caller_kind, minute, count, errors, duration_hist`
- Servis A'nın giden `HTTP · B` çağrıları ile servis B'nin `caller = A` gelen trafiği aynı span çiftinden üretilir → iki ekran her zaman tutarlı.
- `caller` kardinalitesi düşük tutulur: user id / IP asla caller değildir.

## KPI & kök neden

### `sli_1m` (rollup, ingest-writer üretir)
`(org_id, project_id, kpi_id, ts, good UInt64, total UInt64)` — KPI'ın good-event tanımı ingest sırasında değerlendirilir.
Burn rate = `(1 − good/total) / (1 − objective)`; pencereler (5m/30m/1h/6h) bu tablodan toplanır. Retention 35 gün.

### `edges_1m` (rollup)
`(org_id, project_id, ts, parent_service, parent_endpoint, child_kind, child_service, child_ref, calls, errors,
duration_sum, duration_p95)` — span parent→child kenarları. RCA ağacı buradan kurulur; istek başına çağrı sayısı
`calls / parent_calls`. Kardinalite: `child_ref` route template / db+statement fingerprint, ham URL yok.
