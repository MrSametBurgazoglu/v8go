# v8go-fork learnings

Append-only. Findings from working in ~/Projects/v8go (frozen: do not edit GezginWebEngineGo).

## Toolchain (confirmed, todo 1)
- `CC=clang CXX=clang++ go test ./...`. clang 22.1.8, go 1.26.5, Fedora 44.
- libv8.a (143 MB) gitignored at `deps/linux_x86_64/`. libc++ statically merged in.
- `go generate` runs clang-format, which is NOT installed here — hand-format to Chromium style.

## ArrayBuffer + TypedArray byte access (todo 2)
Three non-obvious facts cost real debug time; record them so the next todo doesn't repeat the loop.

1. **V8 14.7 removed `ArrayBuffer::New(Isolate*, void* data, size_t length, ArrayBufferCreationMode)`.**
   The only creation overloads left (deps/include/v8-array-buffer.h ~L257/L274) are:
   - `ArrayBuffer::New(Isolate*, size_t byte_length, init_mode = kZeroInitialized, on_failure = kOutOfMemory)`
   - `ArrayBuffer::New(Isolate*, std::shared_ptr<BackingStore>)`
   `ArrayBufferCreationMode` (kInternalized/kExternalized) still exists in the enum but no longer has
   a `New` overload taking `(data, len, mode)`. To get "V8 owns a copy of my bytes", use the first
   overload then `memcpy(buf->Data(), data, len)`. `Data()` returns a writable pointer for a freshly
   created (non-externalized) buffer. kOutOfMemory means OOM aborts the process — it never returns null,
   so no null check is warranted.

2. **`ArrayBuffer::New` needs an active `Context::Scope` — unlike `Integer::New` / `String::New`.**
   `ISOLATE_SCOPE_INTERNAL_CONTEXT` (v8go.cc ~L142) sets up Locker + Isolate::Scope + HandleScope and
   fetches the internal `m_ctx*`, but does NOT enter `Context::Scope`. `NewValueInteger`/`NewValueString`
   work without it because they create primitives. `ArrayBuffer::New` creates a **JS object**
   (`v8::internal::Factory::NewJSArrayBufferAndBackingStore`), which needs the native context for its
   initial map/prototype lookup. Omitting the scope segfaults deep in V8 (faulting addr 0xffffffffffffffff).
   Fix: mirror the `LOCAL_VALUE` macro (v8go.cc ~L993) — `ISOLATE_SCOPE(iso)` + explicit
   `Context::Scope context_scope(ctx->ptr.Get(iso))`. Any future value *constructor* that builds a JS
   object (Object, Array, Date, Map, Promise, ...) will need the same.

3. **`ObjectTemplate.Set` rejects runtime JS objects.** It accepts only primitives and Templates
   (`Fatal error in v8::Template::Set: Invalid value, must be a primitive or a Template`). To expose a
   runtime-created `*Value` (e.g. an ArrayBuffer from `NewArrayBuffer`) to JS, set it on the **live**
   global object: `ctx.Global().Set("name", val)` — `Object.Set` accepts any `Valuer`.

## API surface notes
- `ArrayBufferView` (base of TypedArray + DataView, in v8-array-buffer.h ~L427) owns `Buffer()`,
  `ByteOffset()`, `ByteLength()`, `CopyContents()`. TypedArray subclasses (Uint8Array etc.) only add
  `New` + `kMaxLength`; all byte geometry is on the base. Cast to `Local<TypedArray>` to use them.
- `BackingStoreData`/`BackingStoreByteLength`/`BackingStoreRelease` (v8go.cc ~L1954) are generic over
  SharedArrayBuffer + ArrayBuffer backing stores — one set of helpers, reused; do not duplicate.
- Zero-copy Go slice over V8 memory: `unsafe.Slice((*byte)(C.BackingStoreData(bs)), C.BackingStoreByteLength(bs))`,
  matching the `SharedArrayBufferGetContents` pattern (value.go ~L587). Lifetime governed by a `release`
  callback the caller must invoke.
- Integer extraction on `*Value` is `Integer() int64` (not `Int64()`). Also: `Int32()`, `Uint32()`,
  `Number() float64`, `ArrayIndex() (uint32, ok)`.

## Test patterns
- External test package `v8go_test`, import as `v8` (see value_test.go).
- `ctx.RunScript(source, origin string)` — exactly 2 args. Bind a JS name across calls with a global
  `var` (`ctx.RunScript("var buf = new ArrayBuffer(16)", ...)`) or `ctx.Global().Set(...)`.
- Raw `ArrayBuffer` is not index-indexed in JS (`buf[0]` is undefined) — read bytes via a view:
  `new Uint8Array(buf)[0]`.
- `t.Parallel()` is used pervasively; isolates are cheap and per-test.
- Pre-existing failure to ignore: `ExampleFunctionTemplate_fetch` (stale network assertion, got "" want HTML).

## Baseline
- After todo 2: 107 PASS / 1 FAIL (`ExampleFunctionTemplate_fetch`). Was 102/1 before todo 2 (+5 new tests).
