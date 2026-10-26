# symbolicator

Turns minified / obfuscated / unsymbolicated stacks from RUM and APM errors back into source locations
(`.ai/contracts.md` §12). Module `github.com/tasktrooper/telemetry/services/symbolicator`, binary `cmd/symbolicator`, port **8095**.

| Platform | Artifact (`kind`) | Input stack |
|---|---|---|
| Web / React Native JS | `sourcemap` — Source Map v3 (`*.js.map`, indexed/sectioned maps too) + optionally the minified `.js` | V8 `at fn (url:line:col)` and Firefox/Safari `fn@url:line:col` |
| Android / JVM | `r8` / `proguard` — `mapping.txt` | `at a.b.c(SourceFile:12)`, `Caused by:` / `Suppressed:` headers |
| iOS / macOS | `dsym` — zipped `.dSYM` bundle or the raw Mach-O DWARF file (fat files OK) | iOS SDK crash report: frames + `Binary Images:` (load address, UUID) |
| Android NDK | `ndk` — unstripped `*.so` (ELF, GNU build id) | tombstone frames `#NN pc <rel hex>  <path> [(fn+off)] (BuildId: <hex>)` |
| Flutter (Dart AOT) | `dart` — `--split-debug-info` `*.symbols` (ELF on Android, Mach-O on iOS) | Dart non-symbolic trace (`build_id`, `#00 abs … virt …`) |

## API

All `/v1` routes need `Authorization: Bearer <api key>` (Postgres `api_keys`, `kind='api'`, sha256 lookup,
60 s cache incl. negative answers; DB down → 503). Dev key: `tt_dev_api_key` (project 1).

- `POST /v1/artifacts` — `multipart/form-data`: fields `kind` (`sourcemap|dsym|r8|proguard|dart|ndk`), `app`, `release`
  (= RUM `resource.version` / `service.version`), one or more file parts (any part name; the SDK uploaders use `file`). The **raw** `filename` is kept as the relative
  name (`assets/app.min.js.map`; `..` is cleaned). Response `201 {kind, app, release, files:[…], errors:[{name,error}]}`;
  `400` when no file was accepted, `413` over the limit. Limits: **200 MB** per debug file (zip entries included,
  zip bombs capped at 4×), **50 MB** per map / JS / mapping.
- `GET /v1/artifacts?app=&release=&kind=` → `{artifacts:[{kind, app, release, name, key, size, sha256, uploadedAt, minified?, debug?:[{id,arch,format}], mapId?}]}`.
- `DELETE /v1/artifacts?app=&release=[&kind=]` → `{deleted:n}` (extension: CLI `delete`, test cleanup).
- `POST /v1/symbolicate` `{app, release, platform, stack}` → `{stack, frames:[{fn, file, line, col, inApp, symbolicated, module?, addr?, mangled?, inlined?, raw?}], symbolicated, platform, missing?}`.
  `platform`: `javascript|web|browser|react-native` · `android|jvm` · `ios|apple|macos` · `flutter|dart`; empty =
  sniffed from the stack. Nothing matched → the raw stack with `symbolicated:false`. `missing` lists debug ids
  (dSYM UUID / Dart build id) of in-app images without an upload. Frames are innermost first; inlined frames are
  expanded (`inlined:true` = inlined into the next frame). Stack ≤ 1 MB.
- `GET /healthz` (no auth; 503 when S3 is unreachable).

## Storage

S3-compatible, path-style (dev: SeaweedFS `http://localhost:9002`, `trooper`/`trooper-secret`); the bucket
(`tt-artifacts`) is created on start. No Postgres tables — every release has an index object:

```
<project>/<kind>/<app>/<release>/<file name>        artifacts (app/release path-escaped)
<project>/<kind>/<app>/<release>/index.json         {files:[{name,key,size,sha256,uploadedAt,minified,minifiedKey,debug,mapId}]}
```

dSYM / Dart files are stored as `<first debug id>/<file name>`, indexed by every UUID/arch slice they contain.
Index writes are serialised per release inside one process (concurrent uploads of the *same* release to two
replicas can lose an index entry; re-upload fixes it). Note: contracts.md §12 names the key
`<project>/<kind>/<release>/<file>`; the extra `<app>` segment avoids collisions between apps sharing a version string.

**Caches:** results in Redis 24 h (`ttsym:res:<sha256(project, app, release, platform, generation, stack)>`);
every upload/delete increments `ttsym:gen:<project>:<app>:<release>`, so cached results never outlive new artifacts.
Redis down → misses. In process: LRU of parsed maps (64), mappings (16), opened debug images (16); debug files are
downloaded to `TT_SYMBOLICATOR_CACHE_DIR`.

## How matching works

- **JS:** the frame URL's path (scheme/host/query dropped; `~/` = site root, like Sentry's `~/` prefix) is matched to
  the minified name of each map: exact path → longest `/<name>` suffix → unique basename. The minified name is the map
  name minus `.map`, the map's `file`, or — when the minified `.js` is uploaded too — its `//# sourceMappingURL`.
  Function names: with the minified source, the declaration of the minified function is located and the map's name
  for that token is used (correct for every frame); otherwise the caller's call-site name; otherwise the minified name.
  `inApp=false` for `node_modules/`, webpack runtime, browser extensions.
- **R8/ProGuard:** class / method / line ranges incl. inline chains (`1:1:…:7:7 -> a` groups, innermost first),
  `a:b` vs single original lines, qualified inlined methods (`com.Other.m()`), `{"id":"sourceFile"}` file names
  and `rewriteFrame` (`removeInnerFrames` on `throws(...)`). Ambiguous names without line info print
  `<OR>` alternatives. Exception class names in headers are deobfuscated. Outlines/residual signatures are ignored.
- **dSYM:** image by address range (fallback name) from `Binary Images:`, debug file by UUID, file address =
  `addr − load + __TEXT.vmaddr` (return addresses −1 except frame 0); DWARF line table + `DW_TAG_subprogram`
  (nested Swift closures, `abstract_origin` / `specification`) + `DW_TAG_inlined_subroutine` call sites;
  symbol table fallback. `inApp` = image not under `/usr/lib`, `/System`, ….
- **Swift names:** `fn` is the demangled (`--simplified`) name when `swift-demangle` runs (`TT_SWIFT_DEMANGLE`,
  `$PATH`, `xcrun --find`); `mangled` always keeps the linkage name. The container image has no demangler.
- **Android NDK:** each `#NN pc` frame is looked up in the `ndk`/`dsym`/`dart` debug file whose ELF build id equals
  its `BuildId`; `pc` is used as-is (the Android SDK already moved frames ≥ 1 to the call instruction: −4 arm64,
  −2 thumb, −1 x86). `/system`, `/apex`, `/vendor` libraries are not in-app. With `platform=android` a stack that has
  tombstone frames goes this way, otherwise through the R8 mapping (mixed Java+native stacks: native part only).
- **iOS release / threads:** the iOS SDK reports every thread (`Thread N:` / `Thread N Crashed:` sections) — all of
  them are symbolicated; its release string is `"<short> (<build>)"` (e.g. `1.4.0 (212)`), the same value
  `sdks/ios/Scripts/upload-dsyms.sh` uploads with, so it must be used verbatim.
- **Flutter:** the `virt` address of each frame (or `abs − isolate_dso_base`) is looked up in the debug file whose
  ELF build id / Mach-O UUID equals the trace's `build_id`. Obfuscated Dart *class/method names in messages* are not
  deobfuscated (`--save-obfuscation-map` is not supported yet).

## Config

| Env | Default |
|---|---|
| `TT_SYMBOLICATOR_ADDR` | `:8095` |
| `TT_S3_ENDPOINT` / `TT_S3_BUCKET` / `TT_S3_ACCESS_KEY` / `TT_S3_SECRET_KEY` / `TT_S3_REGION` | `http://localhost:9002` / `tt-artifacts` / `trooper` / `trooper-secret` / `us-east-1` |
| `TT_POSTGRES_URL`, `TT_REDIS_URL` (`off` disables the cache), `TT_LOG_LEVEL` | see `services/pkg/config` |
| `TT_SYMBOLICATOR_CACHE_DIR` | `$TMPDIR/tt-symbolicator` |
| `TT_SWIFT_DEMANGLE` | auto (`off` disables) |

## CLI

`tools/trooper-cli` (own module, stdlib only): `upload-sourcemaps <dir>`, `upload-dsym <path>…` (zips `.dSYM`
dirs), `upload-mapping <mapping.txt> [--kind proguard]`, `upload-flutter-symbols <path>…`, `upload-ndk-symbols <path>…`, `list`,
`symbolicate --stdin|--file [--platform]`, `delete`; `--app --release`, `TROOPER_API_URL` (default
`http://localhost:8095`), `TROOPER_API_KEY`, `--json`. Exit codes: 0 ok, 1 network/server, 2 usage, 3 unauthorized,
4 some files rejected. `trooper-cli --help` for details.

## Development

```bash
GOWORK=off go test ./...                          # unit + handler tests (in-memory store)
TT_E2E=1 GOWORK=off go test ./e2e/ -v             # live: SeaweedFS + Postgres + Redis, drives the CLI binary
docker build -f symbolicator/Dockerfile -t tasktrooper/symbolicator services/
```

Fixtures in `testdata/` are real build outputs: `js/` (esbuild 0.25 `--bundle --minify --sourcemap` of `js/src`),
`android/mapping.txt` (R8 8.11.18 on `android/src` with `rules.pro`; `stack.txt` is the real obfuscated trace),
`ios/ShopApp.dSYM.zip` (fat arm64+x86_64, `swiftc -g`/`clang -g` + `dsymutil` of `ios/src`; `crash.txt` uses
the lldb return addresses with a 0x4a1c000 slide), `ndk/libshop.so` (`clang -target aarch64-linux-android24 -g -O1 -shared`, lld `--build-id=sha1`, of `ndk/shop.c`;
`tombstone.txt` uses its real call-site pcs), `flutter/` (`dart compile exe` / `aot-snapshot --target-os=linux`
with `--dwarf-stack-traces --obfuscate --save-debugging-info`; `trace-macos.txt` is a real trace).
