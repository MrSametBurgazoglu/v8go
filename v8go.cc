// Copyright 2019 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

#include "v8go.h"

#include <stdio.h>

#include <cstdlib>
#include <cstring>
#include <limits>
#include <iostream>
#include <sstream>
#include <string>
#include <unordered_map>
#include <vector>

#include "_cgo_export.h"
#include "v8-extension.h"
#include "v8-inspector.h"

using namespace v8;

auto default_platform = platform::NewDefaultPlatform();
ArrayBuffer::Allocator* default_allocator;

// BoundedArrayBufferAllocator refuses a request past a ceiling instead of
// letting V8's default allocator take the process down.
//
// V8's default allocator calls FatalProcessOutOfMemory when the underlying
// allocation fails, which is an abort: SIGTRAP, no Go panic, nothing above can
// catch it, and the embedder's whole process goes. That is the right behaviour
// for a shell whose only job is to run one script, and the wrong one for a
// browser, where the allocation size is chosen by the PAGE:
//
//     new Int32Array(536870911)   // two gigabytes
//
// is four lines of script that end the browser. Returning nullptr instead is
// what the ArrayBuffer::Allocator contract asks for on failure, and V8 turns it
// into the RangeError every engine throws: "Array buffer allocation failed".
//
// The ceiling is deliberately generous — a page doing real work with typed
// arrays (a decoder, a wasm heap, an image buffer) stays well under it — and
// the point is not the number but that a limit exists at all and that passing
// it is an exception rather than an abort. Allocate() still tries the real
// allocation below the ceiling and still returns nullptr if THAT fails, so a
// machine genuinely out of memory also throws rather than aborting.
class BoundedArrayBufferAllocator : public ArrayBuffer::Allocator {
 public:
  explicit BoundedArrayBufferAllocator(size_t ceiling) : ceiling_(ceiling) {}

  void* Allocate(size_t length) override {
    if (length > ceiling_) {
      return nullptr;
    }
    // calloc rather than malloc+memset: the contract is zeroed memory, and a
    // failed calloc returns nullptr, which is exactly what we want to pass on.
    return calloc(length, 1);
  }

  void* AllocateUninitialized(size_t length) override {
    if (length > ceiling_) {
      return nullptr;
    }
    return malloc(length);
  }

  void Free(void* data, size_t) override { free(data); }

  // Said up front, so V8 throws its RangeError for a length past the ceiling
  // without trying: a refused Allocate() first makes V8 collect garbage to
  // make room, three times over, and that pressure reached the embedder's
  // near-heap-limit callback -- which ends the page for asking.
  size_t MaxAllocationSize() const override { return ceiling_; }

 private:
  size_t ceiling_;
};

// One gibibyte. Chromium's own limit for a single ArrayBuffer on 64-bit is of
// this order, and a page that needs more than a gigabyte in one buffer is not a
// page a browser can serve anyway.
static const size_t kMaxArrayBufferBytes = 1024ull * 1024ull * 1024ull;

const int ScriptCompilerNoCompileOptions = ScriptCompiler::kNoCompileOptions;
const int ScriptCompilerConsumeCodeCache = ScriptCompiler::kConsumeCodeCache;
const int ScriptCompilerEagerCompile = ScriptCompiler::kEagerCompile;

struct m_ctx {
  Isolate* iso;
  std::unordered_map<long, m_value*> vals;
  std::vector<m_unboundScript*> unboundScripts;
  // In-memory registry of ES module source keyed by the specifier string that
  // JS passes to import(). Consulted by the dynamic-import host hook.
  std::unordered_map<std::string, std::string> modules;
  // The module map the spec requires: one compiled record per specifier, so a
  // specifier is instantiated and evaluated exactly once however many times it
  // is imported. Without it every import() compiled a fresh record and
  // re-evaluated the module — and a module that imports itself (webpack and
  // rspack bundles do, to reach their own runtime) re-entered without bound
  // until the heap was gone.
  std::unordered_map<std::string, Global<Module>> moduleRecords;
  Persistent<Context> ptr;
  long nextValId;
  // use_depth counts the C frames on the stack that are working in this
  // context: an entry (LOCAL_CONTEXT, LOCAL_VALUE and the explicit scopes that
  // take a ContextPtr) or a Go callback dispatched for a function or
  // interceptor the context owns. See ContextInUse.
  int use_depth = 0;
  // freed is set when ContextFree ran while the context was in use: the
  // struct is then deleted by the last ContextUse to leave, not by the free.
  bool freed = false;
  // lastException is the value behind the most recent RtnError the isolate
  // produced, on the isolate's internal context only (ContextTakeException).
  // Weak: an error nobody asks for must not keep its realm alive.
  Global<Value> lastException;
  // importMetaInit is called with (import.meta, url) the first time a module
  // in this context touches import.meta, after `url` is set: the embedder's
  // hook for the rest of the object (HTML's import.meta.resolve).
  // ContextSetImportMetaInitializer.
  Global<Function> importMetaInit;
};

static void contextDelete(m_ctx* ctx);

// ContextUse marks a context in use for the life of one C frame. ContextFree
// deletes the m_ctx and every m_value tracked in it, so an embedder that frees
// a context while one of these is live has pulled the struct out from under a
// caller that will still write through it; ContextInUse is how it asks first.
struct ContextUse {
  explicit ContextUse(m_ctx* ctx) : ctx_(ctx) {
    if (ctx_ != nullptr) {
      ctx_->use_depth++;
    }
  }
  ~ContextUse() {
    if (ctx_ != nullptr && --ctx_->use_depth == 0 && ctx_->freed) {
      contextDelete(ctx_);
    }
  }
  ContextUse(const ContextUse&) = delete;
  ContextUse& operator=(const ContextUse&) = delete;

 private:
  m_ctx* ctx_;
};

struct m_value {
  long id;
  Isolate* iso;
  m_ctx* ctx;
  Global<Value> ptr;
  // weak_token is what a token-weak handle reports when it is collected
  // (ValueSetWeakToken); zero otherwise.
  int64_t weak_token = 0;
};

// contextDelete deletes a context's struct, and any value tracked in it after
// ContextFree emptied it (a deferred free's callers may still mint some).
static void contextDelete(m_ctx* ctx) {
  for (auto it = ctx->vals.begin(); it != ctx->vals.end(); ++it) {
    auto value = it->second;
    value->ptr.Reset();
    delete value;
  }
  ctx->vals.clear();
  delete ctx;
}

struct m_template {
  Isolate* iso;
  // Heap-allocate the persistent so we can intentionally leak the handle in
  // TemplateFreeWrapper when the finalizer may outlive the isolate. V8 14.x
  // removed PersistentBase::Empty() (the old non-disposing clear), so the only
  // way to free the struct without triggering a V8 DCHECK on a dead isolate
  // is to drop the pointer to the persistent rather than destruct it. The
  // handle slot leaks but is reclaimed on Isolate::Dispose.
  Persistent<Template>* ptr;
};

struct m_unboundScript {
  Persistent<UnboundScript> ptr;
};

const char* CopyString(std::string str) {
  int len = str.length();
  char* mem = (char*)malloc(len + 1);
  memcpy(mem, str.data(), len);
  mem[len] = 0;
  return mem;
}

const char* CopyString(String::Utf8Value& value) {
  if (value.length() == 0) {
    return nullptr;
  }
  return CopyString(std::string(*value, value.length()));
}

static RtnError ExceptionError(TryCatch& try_catch,
                               Isolate* iso,
                               Local<Context> ctx) {
  HandleScope handle_scope(iso);

  RtnError rtn = {nullptr, nullptr, nullptr};

  if (try_catch.HasTerminated()) {
    rtn.msg =
        CopyString("ExecutionTerminated: script execution has been terminated");
    return rtn;
  }

  String::Utf8Value exception(iso, try_catch.Exception());
  rtn.msg = CopyString(exception);

  // The thrown value itself, for an embedder that reports it: an ErrorEvent's
  // `error` is the object the script threw, not its text.
  if (m_ctx* ictx = static_cast<m_ctx*>(iso->GetData(0))) {
    ictx->lastException.Reset(iso, try_catch.Exception());
    ictx->lastException.SetWeak();
  }

  Local<Message> msg = try_catch.Message();
  if (!msg.IsEmpty()) {
    rtn.opaque = msg->IsOpaque() ? 1 : 0;
    String::Utf8Value origin(iso, msg->GetScriptOrigin().ResourceName().As<Value>());
    std::ostringstream sb;
    sb << *origin;
    Maybe<int> line = try_catch.Message()->GetLineNumber(ctx);
    if (line.IsJust()) {
      sb << ":" << line.ToChecked();
    }
    Maybe<int> start = try_catch.Message()->GetStartColumn(ctx);
    if (start.IsJust()) {
      sb << ":"
         << start.ToChecked() + 1;  // + 1 to match output from stack trace
    }
    rtn.location = CopyString(sb.str());
  }

  Local<Value> mstack;
  if (try_catch.StackTrace(ctx).ToLocal(&mstack)) {
    String::Utf8Value stack(iso, mstack);
    rtn.stack = CopyString(stack);
  }

  return rtn;
}

m_value* tracked_value(m_ctx* ctx, m_value* val) {
  // (rogchap) we track values against a context so that when the context is
  // closed (either manually or GC'd by Go) we can also release all the
  // values associated with the context;
  if (val->id == 0) {
    val->id = ++ctx->nextValId;
    ctx->vals[val->id] = val;
  }

  return val;
}

m_unboundScript* tracked_unbound_script(m_ctx* ctx, m_unboundScript* us) {
  ctx->unboundScripts.push_back(us);

  return us;
}

extern "C" {

/********** Isolate **********/

#define ISOLATE_SCOPE(iso)           \
  Locker locker(iso);                \
  Isolate::Scope isolate_scope(iso); \
  HandleScope handle_scope(iso);

#define ISOLATE_SCOPE_INTERNAL_CONTEXT(iso) \
  ISOLATE_SCOPE(iso);                       \
  m_ctx* ctx = isolateInternalContext(iso);

void Init() {
#ifdef _WIN32
  V8::InitializeExternalStartupData(".");
#endif
  V8::InitializePlatform(default_platform.get());
  V8::Initialize();

  default_allocator = new BoundedArrayBufferAllocator(kMaxArrayBufferBytes);
  return;
}

// finishIsolateInit performs the isolate setup that is shared between
// NewIsolate and NewIsolateWithOptions: locker / handle scope, capture
// stack traces, and an internal Context registered as slot 0 data so
// later cgo entrypoints can recover it via isolateInternalContext.
//
// It also registers the dynamic-import and import.meta host hooks. These
// fire only when JS actually uses import() or import.meta, so registering
// them unconditionally is free for isolates that never touch modules.
static MaybeLocal<Promise> hostImportModuleDynamically(
    Local<Context> context, Local<Data> host_defined_options,
    Local<Value> resource_name, Local<String> specifier,
    Local<FixedArray> import_attributes);
static void hostInitializeImportMeta(Local<Context> context,
                                     Local<Module> module,
                                     Local<Object> meta);
static void finishIsolateInit(Isolate* iso) {
  Locker locker(iso);
  Isolate::Scope isolate_scope(iso);
  HandleScope handle_scope(iso);

  iso->SetCaptureStackTraceForUncaughtExceptions(true);
  iso->SetHostImportModuleDynamicallyCallback(hostImportModuleDynamically);
  iso->SetHostInitializeImportMetaObjectCallback(hostInitializeImportMeta);

  m_ctx* ctx = new m_ctx;
  ctx->ptr.Reset(iso, Context::New(iso));
  ctx->iso = iso;
  iso->SetData(0, ctx);
}

IsolatePtr NewIsolate() {
  Isolate::CreateParams params;
  params.array_buffer_allocator = default_allocator;
  Isolate* iso = Isolate::New(params);
  finishIsolateInit(iso);
  return iso;
}

// NewIsolateWithOptions surfaces v8::Isolate::CreateParams::constraints so
// callers can set the initial / max old-generation and max young-generation
// sizes per isolate. Setting initial_old_space_bytes makes V8 commit the
// requested size at isolate creation rather than growing on demand — this
// matters on Windows under memory pressure where peak-time VirtualAlloc
// can be denied. See docs/v8-windows-oom.md in the deskbot repo.
//
// Any opts field set to 0 is left at the V8 default. Existing callers can
// continue using NewIsolate() with no behavior change.
IsolatePtr NewIsolateWithOptions(IsolateOptions opts) {
  Isolate::CreateParams params;
  params.array_buffer_allocator = default_allocator;
  if (opts.max_old_space_bytes > 0) {
    params.constraints.set_max_old_generation_size_in_bytes(
        opts.max_old_space_bytes);
  }
  if (opts.initial_old_space_bytes > 0) {
    params.constraints.set_initial_old_generation_size_in_bytes(
        opts.initial_old_space_bytes);
  }
  if (opts.max_young_space_bytes > 0) {
    params.constraints.set_max_young_generation_size_in_bytes(
        opts.max_young_space_bytes);
  }
  Isolate* iso = Isolate::New(params);
  finishIsolateInit(iso);
  return iso;
}

static inline m_ctx* isolateInternalContext(Isolate* iso) {
  return static_cast<m_ctx*>(iso->GetData(0));
}

void IsolatePerformMicrotaskCheckpoint(IsolatePtr iso) {
  ISOLATE_SCOPE(iso)
  iso->PerformMicrotaskCheckpoint();
}

void IsolateSetMicrotasksPolicy(IsolatePtr iso, int policy) {
  if (iso == nullptr) {
    return;
  }
  ISOLATE_SCOPE(iso)
  iso->SetMicrotasksPolicy(policy != 0 ? v8::MicrotasksPolicy::kExplicit
                                        : v8::MicrotasksPolicy::kAuto);
}

struct contextExtensions;
static std::unordered_map<Isolate*, contextExtensions>& isolateExtensions();

void IsolateDispose(IsolatePtr iso) {
  if (iso == nullptr) {
    return;
  }
  ContextFree(isolateInternalContext(iso));
  // A later isolate can be allocated at the same address.
  isolateExtensions().erase(iso);

  iso->Dispose();
}

void IsolateTerminateExecution(IsolatePtr iso) {
  iso->TerminateExecution();
}

int IsolateIsExecutionTerminating(IsolatePtr iso) {
  return iso->IsExecutionTerminating();
}

// nearHeapLimitTrampoline is V8's required signature; it forwards to the
// Go-side callback exported by isolate.go (goNearHeapLimitCallback). The
// callback may return current_heap_limit unchanged (V8 will then OOM as
// usual), or a larger value to grow the cap, or a smaller value to allow
// V8 to restore the limit later.
size_t nearHeapLimitTrampoline(void* data,
                                size_t current_heap_limit,
                                size_t initial_heap_limit) {
  IsolatePtr iso = static_cast<IsolatePtr>(data);
  return goNearHeapLimitCallback(iso,
                                  current_heap_limit,
                                  initial_heap_limit);
}

void IsolateAddNearHeapLimitCallback(IsolatePtr iso) {
  if (iso == nullptr) {
    return;
  }
  // Pass the IsolatePtr as the data so the Go callback can identify which
  // isolate is firing without us maintaining a separate registry.
  iso->AddNearHeapLimitCallback(nearHeapLimitTrampoline,
                                 static_cast<void*>(iso));
}

void IsolateRemoveNearHeapLimitCallback(IsolatePtr iso, size_t heap_limit) {
  if (iso == nullptr) {
    return;
  }
  iso->RemoveNearHeapLimitCallback(nearHeapLimitTrampoline, heap_limit);
}

void IsolateAutomaticallyRestoreInitialHeapLimit(IsolatePtr iso,
                                                  double threshold) {
  if (iso == nullptr) {
    return;
  }
  iso->AutomaticallyRestoreInitialHeapLimit(threshold);
}

// promiseRejectTrampoline has the signature V8 requires
// (void(*)(PromiseRejectMessage)). It is registered per-isolate via
// Isolate::SetPromiseRejectCallback and fires whenever a promise is
// rejected (event kPromiseRejectWithNoHandler) or a handler is attached
// after rejection (kPromiseHandlerAddedAfterReject), etc.
//
// V8 invokes the callback during JS execution on the firing isolate, so
// Isolate::GetCurrent() recovers it without a data parameter (the V8
// callback type has none). The rejection value is wrapped in a transient
// m_value whose Global<Value> keeps it alive for the Go callback's
// synchronous read. The wrapper is freed immediately after the callback
// returns — callers must NOT retain the ValuePtr.
void promiseRejectTrampoline(PromiseRejectMessage msg) {
  Isolate* iso = Isolate::GetCurrent();
  ISOLATE_SCOPE(iso)
  m_ctx* ctx = isolateInternalContext(iso);
  Local<Context> local_ctx = ctx->ptr.Get(iso);
  Context::Scope context_scope(local_ctx);

  // V8 gives no value for kPromiseHandlerAddedAfterReject; undefined stands
  // in, so a callback may read it without meeting an empty handle.
  Local<Value> reason = msg.GetValue();
  if (reason.IsEmpty()) {
    reason = Undefined(iso);
  }
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, reason);

  // The promise itself, transient like the value, and the ref of the context
  // it was created in (0 when that is not a context v8go made): what an
  // embedder needs to keep the HTML standard's per-global lists of rejected
  // promises, and to fire unhandledrejection/rejectionhandled with the
  // promise the page holds rather than a description of it.
  Local<Promise> promise = msg.GetPromise();
  m_value* prom = new m_value;
  prom->id = 0;
  prom->iso = iso;
  prom->ctx = ctx;
  prom->ptr = Global<Value>(iso, promise);
  int ctx_ref = 0;
  Local<Context> creation;
  if (!promise.IsEmpty() &&
      promise->GetCreationContext(iso).ToLocal(&creation)) {
    Local<Value> ref_val = creation->GetEmbedderData(1);
    if (!ref_val.IsEmpty() && ref_val->IsInt32()) {
      ctx_ref = ref_val.As<Integer>()->Value();
    }
  }

  goPromiseRejectCallback(iso, static_cast<int>(msg.GetEvent()), val, prom,
                          ctx_ref);

  val->ptr.Reset();
  delete val;
  prom->ptr.Reset();
  delete prom;
}

}  // extern "C" -- the trampoline returns a C++ type

// codeGenerationTrampoline is V8's ModifyCodeGenerationFromStringsCallback2. V8
// asks it only for a context whose AllowCodeGenerationFromStrings is false, so
// the answer is always "refuse"; the call exists so the embedder hears of the
// refusal (a Content Security Policy violation report) with the context's ref.
ModifyCodeGenerationFromStringsResult codeGenerationTrampoline(
    Local<Context> context,
    Local<Value> source,
    bool is_code_like) {
  // eval of a non-string is the identity and compiles nothing: allowed, with
  // no source, which is what makes V8 hand the value back (Chromium's answer,
  // and the order the standard's eval-typecheck-callout test pins).
  if (source.IsEmpty() || !source->IsString()) {
    ModifyCodeGenerationFromStringsResult identity;
    identity.codegen_allowed = true;
    return identity;
  }
  int ctx_ref = 0;
  Local<Value> ref_val = context->GetEmbedderData(1);
  if (!ref_val.IsEmpty() && ref_val->IsInt32()) {
    ctx_ref = ref_val.As<Integer>()->Value();
  }
  goCodeGenerationRefused(Isolate::GetCurrent(), ctx_ref);
  return ModifyCodeGenerationFromStringsResult();
}

extern "C" {

void IsolateSetCodeGenerationRefusedCallback(IsolatePtr iso) {
  if (iso == nullptr) {
    return;
  }
  ISOLATE_SCOPE(iso)
  iso->SetModifyCodeGenerationFromStringsCallback(codeGenerationTrampoline);
}

void IsolateSetPromiseRejectCallback(IsolatePtr iso) {
  if (iso == nullptr) {
    return;
  }
  ISOLATE_SCOPE(iso)
  iso->SetPromiseRejectCallback(promiseRejectTrampoline);
}

/********** Dynamic import() and import.meta host hooks **********/

// recoverModuleContext maps the firing Local<Context> back to its m_ctx via
// the same ctx_ref slot the FunctionTemplate callback uses (embedder data
// slot 1). Returns nullptr for contexts v8go did not create.
static m_ctx* recoverModuleContext(Local<Context> context) {
  Local<Value> ref_val = context->GetEmbedderData(1);
  if (ref_val.IsEmpty() || !ref_val->IsInt32()) {
    return nullptr;
  }
  return goContext(ref_val.As<Integer>()->Value());
}

// compileRegistryModule looks up |specifier| in the context's module registry
// and returns a freshly-compiled, uninstantiated Module. An empty MaybeLocal
// means miss or compile failure; for nested static imports V8 turns that into
// a SyntaxError at instantiation time.
static MaybeLocal<Module> compileRegistryModule(Local<Context> context,
                                                Local<String> specifier) {
  Isolate* iso = Isolate::GetCurrent();
  m_ctx* ctx = recoverModuleContext(context);
  if (ctx == nullptr) {
    return MaybeLocal<Module>();
  }
  String::Utf8Value spec_utf8(iso, specifier);
  const char* spec_cstr = *spec_utf8 ? *spec_utf8 : "";
  // The module map first: a specifier already compiled resolves to the same
  // record, which is what makes a repeated import share one evaluation
  // instead of starting another.
  auto cached = ctx->moduleRecords.find(spec_cstr);
  if (cached != ctx->moduleRecords.end()) {
    return cached->second.Get(iso);
  }
  auto it = ctx->modules.find(spec_cstr);
  if (it == ctx->modules.end()) {
    return MaybeLocal<Module>();
  }
  Local<String> src;
  if (!String::NewFromUtf8(iso, it->second.data(), NewStringType::kNormal,
                           static_cast<int>(it->second.size()))
           .ToLocal(&src)) {
    return MaybeLocal<Module>();
  }
  // is_module=true is the 9th positional ScriptOrigin argument; CompileModule
  // requires a module-flagged origin.
  ScriptOrigin origin(specifier, 0, 0, false, -1, Local<Value>(), false, false,
                      true);
  ScriptCompiler::Source sc_source(src, origin);
  Local<Module> module;
  if (!ScriptCompiler::CompileModule(iso, &sc_source).ToLocal(&module)) {
    return MaybeLocal<Module>();
  }
  // Recorded before instantiation, so a module that imports itself finds the
  // record already there rather than compiling a second one.
  ctx->moduleRecords[spec_cstr].Reset(iso, module);
  return module;
}

// resolveModuleCallback satisfies Module::InstantiateModule for nested static
// imports inside a registered module. V8 drives the recursive instantiation;
// this callback only hands back a compiled module for each request.
static MaybeLocal<Module> resolveModuleCallback(
    Local<Context> context, Local<String> specifier,
    Local<FixedArray> /*import_attributes*/, Local<Module> /*referrer*/) {
  MaybeLocal<Module> module = compileRegistryModule(context, specifier);
  // A miss throws nothing of its own, and V8 then fails the instantiation
  // with no exception at all: import() had nothing to reject with.
  Isolate* iso = Isolate::GetCurrent();
  if (module.IsEmpty() && !iso->HasPendingException()) {
    String::Utf8Value spec_utf8(iso, specifier);
    std::string message = "Cannot find module '";
    message += *spec_utf8 ? *spec_utf8 : "";
    message += "'";
    iso->ThrowException(Exception::TypeError(
        String::NewFromUtf8(iso, message.c_str(), NewStringType::kNormal)
            .ToLocalChecked()));
  }
  return module;
}

// caughtOrError is what a failed step rejects import() with: the exception it
// threw, or an Error when it failed without one (a context closed before a
// deferred evaluation ran has no module registry left to compile from).
static Local<Value> caughtOrError(Isolate* iso, TryCatch& try_catch) {
  if (try_catch.HasCaught() && !try_catch.Exception().IsEmpty()) {
    return try_catch.Exception();
  }
  return Exception::Error(
      String::NewFromUtf8Literal(iso, "the module could not be evaluated"));
}

// evaluateDynamicImport is import()'s second half: the registry module under
// |key| compiled (or found in the module map), instantiated, evaluated, and
// the promise settled with its namespace or its error.
static MaybeLocal<Promise> evaluateDynamicImport(
    Local<Context> context, Local<Promise::Resolver> resolver,
    Local<String> specifier, TryCatch& try_catch) {
  Local<Module> module;
  if (!compileRegistryModule(context, specifier).ToLocal(&module)) {
    resolver
        ->Reject(context, caughtOrError(Isolate::GetCurrent(), try_catch))
        .Check();
    return resolver->GetPromise();
  }

  // A record already past instantiation is not instantiated again, and one
  // already evaluating or evaluated is not evaluated again — that is the
  // whole point of the module map, and it is what stops a self-import from
  // re-entering. An evaluating module (the cycle case) resolves with its
  // namespace, which is the live binding object the spec hands back.
  Module::Status status = module->GetStatus();
  if (status == Module::kErrored) {
    resolver->Reject(context, module->GetException()).Check();
    return resolver->GetPromise();
  }
  if (status == Module::kUninstantiated) {
    if (!module->InstantiateModule(context, resolveModuleCallback)
             .FromMaybe(false)) {
      resolver
          ->Reject(context, caughtOrError(Isolate::GetCurrent(), try_catch))
          .Check();
      return resolver->GetPromise();
    }
    status = module->GetStatus();
  }

  // Evaluate returns a Promise per spec. A module without top-level await
  // settles it synchronously; a module-level throw lands in kErrored (not in
  // try_catch), so the status must be inspected explicitly.
  //
  // A module with top-level await (or one importing such a module) settles
  // it later, and import() settles with it: resolving with the namespace at
  // once handed out bindings still in their TDZ and swallowed a rejected
  // await. An evaluated module is evaluated again for the same reason, as
  // HTML's import() does: Evaluate answers the promise its evaluation
  // already has, which is still pending while an await in it is.
  if (status == Module::kInstantiated || status == Module::kEvaluated) {
    Local<Value> evaluation;
    if (!module->Evaluate(context).ToLocal(&evaluation)) {
      resolver
          ->Reject(context, caughtOrError(Isolate::GetCurrent(), try_catch))
          .Check();
      return resolver->GetPromise();
    }
    if (module->GetStatus() == Module::kErrored) {
      resolver->Reject(context, module->GetException()).Check();
      return resolver->GetPromise();
    }
    if (evaluation->IsPromise()) {
      Local<Promise> promise = evaluation.As<Promise>();
      switch (promise->State()) {
        case Promise::kRejected:
          resolver->Reject(context, promise->Result()).Check();
          return resolver->GetPromise();
        case Promise::kPending: {
          Isolate* iso = Isolate::GetCurrent();
          Local<Array> data = Array::New(iso, 2);
          data->Set(context, 0, resolver).Check();
          data->Set(context, 1, module->GetModuleNamespace()).Check();
          Local<Function> fulfilled;
          Local<Function> rejected;
          if (!Function::New(
                   context,
                   [](const FunctionCallbackInfo<Value>& info) {
                     Local<Context> c = info.GetIsolate()->GetCurrentContext();
                     Local<Array> d = info.Data().As<Array>();
                     d->Get(c, 0)
                         .ToLocalChecked()
                         .As<Promise::Resolver>()
                         ->Resolve(c, d->Get(c, 1).ToLocalChecked())
                         .Check();
                   },
                   data)
                   .ToLocal(&fulfilled) ||
              !Function::New(
                   context,
                   [](const FunctionCallbackInfo<Value>& info) {
                     Local<Context> c = info.GetIsolate()->GetCurrentContext();
                     Local<Array> d = info.Data().As<Array>();
                     d->Get(c, 0)
                         .ToLocalChecked()
                         .As<Promise::Resolver>()
                         ->Reject(c, info[0])
                         .Check();
                   },
                   data)
                   .ToLocal(&rejected) ||
              promise->Then(context, fulfilled, rejected).IsEmpty()) {
            Local<Value> failure = try_catch.HasCaught()
                                       ? try_catch.Exception()
                                       : Undefined(iso).As<Value>();
            resolver->Reject(context, failure).Check();
          }
          return resolver->GetPromise();
        }
        default:
          break;
      }
    }
  }

  resolver->Resolve(context, module->GetModuleNamespace()).Check();
  return resolver->GetPromise();
}

// deferDynamicImport runs evaluateDynamicImport as a microtask rather than
// inside the import() call. HTML's import() links and evaluates in a reaction
// to the load promise, so a module importing a sibling the graph has fetched
// but not yet evaluated finishes its own body first: evaluating at once ran
// the sibling ahead of the importer, out of the graph's depth-first order.
static MaybeLocal<Promise> deferDynamicImport(Local<Context> context,
                                              Local<Promise::Resolver> resolver,
                                              Local<String> key) {
  Isolate* iso = Isolate::GetCurrent();
  Local<Array> data = Array::New(iso, 2);
  data->Set(context, 0, resolver).Check();
  data->Set(context, 1, key).Check();
  Local<Function> task;
  if (!Function::New(
           context,
           [](const FunctionCallbackInfo<Value>& info) {
             Isolate* iso = info.GetIsolate();
             Local<Context> c = iso->GetCurrentContext();
             Local<Array> d = info.Data().As<Array>();
             Local<Promise::Resolver> r =
                 d->Get(c, 0).ToLocalChecked().As<Promise::Resolver>();
             Local<String> k = d->Get(c, 1).ToLocalChecked().As<String>();
             TryCatch try_catch(iso);
             evaluateDynamicImport(c, r, k, try_catch);
           },
           data)
           .ToLocal(&task)) {
    return MaybeLocal<Promise>();
  }
  iso->EnqueueMicrotask(task);
  return resolver->GetPromise();
}

// deferDynamicImportFailure rejects import() from a microtask too, so calls
// settle in the order they were made whether they fail or not.
static MaybeLocal<Promise> deferDynamicImportFailure(
    Local<Context> context,
    Local<Promise::Resolver> resolver,
    Local<Value> error) {
  Isolate* iso = Isolate::GetCurrent();
  Local<Array> data = Array::New(iso, 2);
  data->Set(context, 0, resolver).Check();
  data->Set(context, 1, error).Check();
  Local<Function> task;
  if (!Function::New(
           context,
           [](const FunctionCallbackInfo<Value>& info) {
             Local<Context> c = info.GetIsolate()->GetCurrentContext();
             Local<Array> d = info.Data().As<Array>();
             d->Get(c, 0)
                 .ToLocalChecked()
                 .As<Promise::Resolver>()
                 ->Reject(c, d->Get(c, 1).ToLocalChecked())
                 .Check();
           },
           data)
           .ToLocal(&task)) {
    resolver->Reject(context, error).Check();
    return resolver->GetPromise();
  }
  iso->EnqueueMicrotask(task);
  return resolver->GetPromise();
}

// hostImportModuleDynamically is the isolate-level host hook fired by
// `await import(specifier)`. For an in-memory registry the module resolves
// synchronously: compile -> instantiate -> evaluate -> resolve the returned
// promise with the module namespace.
static MaybeLocal<Promise> hostImportModuleDynamically(
    Local<Context> context, Local<Data> /*host_defined_options*/,
    Local<Value> resource_name, Local<String> specifier,
    Local<FixedArray> import_attributes) {
  Isolate* iso = Isolate::GetCurrent();
  ISOLATE_SCOPE(iso)
  Context::Scope context_scope(context);
  TryCatch try_catch(iso);

  Local<Promise::Resolver> resolver;
  if (!Promise::Resolver::New(context).ToLocal(&resolver)) {
    return MaybeLocal<Promise>();
  }

  String::Utf8Value spec_utf8(iso, specifier);
  const char* spec_cstr = *spec_utf8 ? *spec_utf8 : "";
  m_ctx* ctx = recoverModuleContext(context);
  // The embedder's resolver below is Go, and it runs with this context's
  // module registry in hand.
  ContextUse ctx_use(ctx);
  // The embedder's import() resolver, when it has one, answers every call
  // with its import attributes: the registry key of the module it names (a
  // module is its URL AND its type, so one URL can be two keys), or an error
  // of the kind HTML specifies, which the promise is rejected with.
  if (ctx != nullptr) {
    std::vector<std::string> attr_strings;
    if (!import_attributes.IsEmpty()) {
      int length = import_attributes->Length();
      for (int i = 0; i + 1 < length; i += 2) {
        Local<Data> key_data = import_attributes->Get(i);
        Local<Data> value_data = import_attributes->Get(i + 1);
        if (!key_data->IsValue() || !value_data->IsValue()) {
          continue;
        }
        String::Utf8Value key_utf8(iso, key_data.As<Value>());
        String::Utf8Value value_utf8(iso, value_data.As<Value>());
        attr_strings.emplace_back(*key_utf8 ? *key_utf8 : "");
        attr_strings.emplace_back(*value_utf8 ? *value_utf8 : "");
      }
    }
    std::vector<char*> attr_ptrs;
    for (auto& a : attr_strings) {
      attr_ptrs.push_back(const_cast<char*>(a.c_str()));
    }
    String::Utf8Value referrer_utf8(iso, resource_name);
    const char* referrer_cstr = *referrer_utf8 ? *referrer_utf8 : "";
    char* out_key = nullptr;
    int out_err_kind = 0;
    char* out_err_msg = nullptr;
    int handled = goResolveDynamicImport(
        ctx, const_cast<char*>(spec_cstr), const_cast<char*>(referrer_cstr),
        attr_ptrs.empty() ? nullptr : attr_ptrs.data(),
        static_cast<int>(attr_strings.size() / 2), &out_key, &out_err_kind,
        &out_err_msg);
    if (handled == 2) {
      Local<String> msg =
          String::NewFromUtf8(iso, out_err_msg ? out_err_msg : "",
                              NewStringType::kNormal)
              .ToLocalChecked();
      free(out_err_msg);
      Local<Value> error;
      switch (out_err_kind) {
        case 1:
          error = Exception::TypeError(msg);
          break;
        case 2:
          error = Exception::SyntaxError(msg);
          break;
        default:
          error = Exception::Error(msg);
      }
      return deferDynamicImportFailure(context, resolver, error);
    }
    if (handled == 1) {
      std::string key = out_key ? std::string(out_key) : std::string(spec_cstr);
      free(out_key);
      if (ctx->modules.find(key) == ctx->modules.end() &&
          ctx->moduleRecords.find(key) == ctx->moduleRecords.end()) {
        std::string errmsg = "Failed to fetch dynamically imported module: ";
        errmsg += spec_cstr;
        return deferDynamicImportFailure(
            context, resolver,
            Exception::TypeError(String::NewFromUtf8(iso, errmsg.c_str(),
                                                     NewStringType::kNormal)
                                     .ToLocalChecked()));
      }
      Local<String> key_str;
      if (!String::NewFromUtf8(iso, key.c_str(), NewStringType::kNormal)
               .ToLocal(&key_str)) {
        return MaybeLocal<Promise>();
      }
      return deferDynamicImport(context, resolver, key_str);
    }
  }
  // A specifier the registry has never seen is offered to the embedder's
  // resolver before it is called missing: a bundler's runtime composes chunk
  // URLs as it goes, so the registry cannot be complete in advance. The
  // resolver fetches and registers, and the second lookup finds it.
  if (ctx != nullptr && ctx->modules.find(spec_cstr) == ctx->modules.end()) {
    String::Utf8Value referrer_utf8(iso, resource_name);
    const char* referrer_cstr = *referrer_utf8 ? *referrer_utf8 : "";
    goResolveModule(ctx, const_cast<char*>(spec_cstr),
                    const_cast<char*>(referrer_cstr));
  }
  if (ctx == nullptr ||
      ctx->modules.find(spec_cstr) == ctx->modules.end()) {
    std::string errmsg = "Cannot find module '";
    errmsg += spec_cstr;
    errmsg += "'";
    Local<String> msg;
    if (!String::NewFromUtf8(iso, errmsg.c_str(), NewStringType::kNormal)
             .ToLocal(&msg)) {
      return MaybeLocal<Promise>();
    }
    return deferDynamicImportFailure(context, resolver, Exception::Error(msg));
  }

  return deferDynamicImport(context, resolver, specifier);
}

// hostInitializeImportMeta is fired the first time a module touches
// import.meta. We populate `url` from the resource_name the module was
// compiled with (the specifier), matching how browsers set it to the
// module's resolved URL.
static void hostInitializeImportMeta(Local<Context> context,
                                     Local<Module> module,
                                     Local<Object> meta) {
  Isolate* iso = Isolate::GetCurrent();
  ISOLATE_SCOPE(iso)
  Context::Scope context_scope(context);

  Local<Value> resource = module->GetResourceName();
  Local<Value> url_val;
  if (resource.IsEmpty() || resource->IsUndefined()) {
    url_val = String::NewFromUtf8(iso, "v8go://module", NewStringType::kNormal)
                  .ToLocalChecked();
  } else {
    url_val = resource;
  }
  Local<String> url_key;
  if (!String::NewFromUtf8(iso, "url", NewStringType::kNormal)
           .ToLocal(&url_key)) {
    return;
  }
  meta->CreateDataProperty(context, url_key, url_val).FromMaybe(false);

  m_ctx* ctx = recoverModuleContext(context);
  if (ctx == nullptr || ctx->importMetaInit.IsEmpty()) {
    return;
  }
  Local<Function> init = ctx->importMetaInit.Get(iso);
  Local<Value> args[2] = {meta, url_val};
  // An initializer that throws leaves import.meta as far as it got; the
  // module that touched import.meta must not see the embedder's exception.
  TryCatch try_catch(iso);
  init->Call(context, Undefined(iso), 2, args).IsEmpty();
}

// ContextRegisterModule stores ES module source under a specifier string so
// the dynamic-import host hook can compile it. Registered on the context; the
// hook recovers the same context via the ctx_ref embedder slot.
void ContextRegisterModule(ContextPtr ctx, const char* specifier,
                           const char* source, int source_len) {
  // By length: a module's text may hold U+0000, which a C string would end at.
  ctx->modules[specifier] = std::string(source, source_len);
  // New source under a specifier retires the record compiled from the old
  // source; otherwise a second document would import the previous page's
  // module.
  auto record = ctx->moduleRecords.find(specifier);
  if (record != ctx->moduleRecords.end()) {
    record->second.Reset();
    ctx->moduleRecords.erase(record);
  }
}

// IsolateWarmupOldGenerationHeap forces V8 to commit ~target_bytes of
// old-generation pages by allocating an Array of distinct one-byte
// strings totaling that size, then dropping the reference and running
// LowMemoryNotification (which performs a full Mark-Compact). With
// --no-memory-reducer set, the freed pages are NOT returned to the OS —
// they remain available for subsequent allocations.
//
// Why distinct strings: V8 deduplicates "X".repeat(N) and similar
// expressions; without distinct content V8 retains a single underlying
// String. We build per-iteration content using a counter so each chunk
// is unique.
//
// The work runs on an internal context attached to the isolate (slot 0
// data), so callers don't need to pass one.
int IsolateWarmupOldGenerationHeap(IsolatePtr iso, size_t target_bytes) {
  if (iso == nullptr || target_bytes == 0) {
    return 0;
  }
  ISOLATE_SCOPE_INTERNAL_CONTEXT(iso);
  Local<Context> local_ctx = ctx->ptr.Get(iso);
  Context::Scope context_scope(local_ctx);
  TryCatch try_catch(iso);

  // Each chunk is 1 MiB of unique bytes; total iterations = target / 1MB.
  // The buffer is built then dropped before the GC, so peak working set
  // is ~target_bytes plus per-string V8 metadata.
  //
  // String construction uses ASCII per-character and Array.from + join
  // to avoid the O(N^2) trap of `s += ...`. Per-iteration content is
  // unique (offset by chunk index) so V8's string deduplication does
  // not collapse the buffer to a single underlying allocation.
  const size_t kChunkBytes = 1u << 20;  // 1 MiB
  size_t chunks = (target_bytes + kChunkBytes - 1) / kChunkBytes;
  std::ostringstream src;
  src << "(function(){var __wbuf__=[];"
      << "var __n__=" << kChunkBytes << ";"
      << "for(var i=0;i<" << chunks << ";i++){"
      << "var arr=new Array(__n__);"
      << "var off=(i*7)&0x7F;"
      << "for(var j=0;j<__n__;j++)arr[j]=String.fromCharCode((j+off)&0x7F);"
      << "__wbuf__.push(arr.join(''));"
      << "}"
      << "__wbuf__=null;"
      << "})();";
  std::string code = src.str();
  Local<String> source;
  if (!String::NewFromUtf8(iso, code.c_str(), NewStringType::kNormal,
                           static_cast<int>(code.size()))
           .ToLocal(&source)) {
    return 1;
  }
  Local<Script> script;
  if (!Script::Compile(local_ctx, source).ToLocal(&script)) {
    return 2;
  }
  Local<Value> result;
  if (!script->Run(local_ctx).ToLocal(&result)) {
    return 3;
  }
  // We deliberately do NOT call LowMemoryNotification here. That call
  // bypasses --no-memory-reducer and decommits the freshly committed
  // pages back to the OS — defeating the entire warmup. By dropping the
  // __wbuf__ reference inside the script and letting V8's normal GC
  // reclaim the strings later, the underlying old-space pages stay in
  // V8's free list ready for the next allocation. With
  // --no-memory-reducer set globally, V8 retains those pages between
  // calls instead of returning them to the OS.
  return 0;
}

IsolateHStatistics IsolationGetHeapStatistics(IsolatePtr iso) {
  if (iso == nullptr) {
    return IsolateHStatistics{0};
  }
  v8::HeapStatistics hs;
  iso->GetHeapStatistics(&hs);

  return IsolateHStatistics{hs.total_heap_size(),
                            hs.total_heap_size_executable(),
                            hs.total_physical_size(),
                            hs.total_available_size(),
                            hs.used_heap_size(),
                            hs.heap_size_limit(),
                            hs.malloced_memory(),
                            hs.external_memory(),
                            hs.peak_malloced_memory(),
                            hs.number_of_native_contexts(),
                            hs.number_of_detached_contexts()};
}

RtnUnboundScript IsolateCompileUnboundScript(IsolatePtr iso,
                                             const char* s,
                                             int s_len,
                                             const char* o,
                                             CompileOptions opts) {
  ISOLATE_SCOPE_INTERNAL_CONTEXT(iso);
  TryCatch try_catch(iso);
  Local<Context> local_ctx = ctx->ptr.Get(iso);
  Context::Scope context_scope(local_ctx);

  RtnUnboundScript rtn = {};

  Local<String> src =
      String::NewFromUtf8(iso, s, NewStringType::kNormal, s_len)
          .ToLocalChecked();
  Local<String> ogn =
      String::NewFromUtf8(iso, o, NewStringType::kNormal).ToLocalChecked();

  ScriptCompiler::CompileOptions option =
      static_cast<ScriptCompiler::CompileOptions>(opts.compileOption);

  ScriptCompiler::CachedData* cached_data = nullptr;

  if (opts.cachedData.data) {
    cached_data = new ScriptCompiler::CachedData(opts.cachedData.data,
                                                 opts.cachedData.length);
  }

  // An opaque script is one the embedder fetched from another origin without
  // CORS: the message of anything it throws says so (Message::IsOpaque), and
  // the embedder mutes what it reports. Not shared cross-origin either, as
  // Blink compiles such a script.
  ScriptOrigin script_origin(ogn, 0, 0, false, -1, Local<Value>(),
                             opts.opaque != 0);

  ScriptCompiler::Source source(src, script_origin, cached_data);

  Local<UnboundScript> unbound_script;
  if (!ScriptCompiler::CompileUnboundScript(iso, &source, option)
           .ToLocal(&unbound_script)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  };

  if (cached_data) {
    rtn.cachedDataRejected = cached_data->rejected;
  }

  m_unboundScript* us = new m_unboundScript;
  us->ptr.Reset(iso, unbound_script);
  rtn.ptr = tracked_unbound_script(ctx, us);
  return rtn;
}

// IsolateCheckModuleSyntax parses s as a module and keeps nothing: what an
// embedder that fetches a module graph itself needs to know before it fetches
// a module's imports, since a module that does not parse has none.
RtnError IsolateCheckModuleSyntax(IsolatePtr iso,
                                  const char* s,
                                  int s_len,
                                  const char* o) {
  ISOLATE_SCOPE_INTERNAL_CONTEXT(iso);
  TryCatch try_catch(iso);
  Local<Context> local_ctx = ctx->ptr.Get(iso);
  Context::Scope context_scope(local_ctx);

  RtnError rtn = {nullptr, nullptr, nullptr};
  Local<String> src;
  Local<String> ogn;
  if (!String::NewFromUtf8(iso, s, NewStringType::kNormal, s_len)
           .ToLocal(&src) ||
      !String::NewFromUtf8(iso, o, NewStringType::kNormal).ToLocal(&ogn)) {
    return rtn;
  }
  ScriptOrigin origin(ogn, 0, 0, false, -1, Local<Value>(), false, false,
                      true);
  ScriptCompiler::Source source(src, origin);
  Local<Module> module;
  if (!ScriptCompiler::CompileModule(iso, &source).ToLocal(&module)) {
    rtn = ExceptionError(try_catch, iso, local_ctx);
  }
  return rtn;
}

/********** Exceptions & Errors **********/

ValuePtr IsolateThrowException(IsolatePtr iso, ValuePtr value) {
  ISOLATE_SCOPE(iso);
  m_ctx* ctx = value->ctx;

  Local<Value> throw_ret_val = iso->ThrowException(value->ptr.Get(iso));

  m_value* new_val = new m_value;
  new_val->id = 0;
  new_val->iso = iso;
  new_val->ctx = ctx;
  new_val->ptr =
      Global<Value>(iso, throw_ret_val);

  return tracked_value(ctx, new_val);
}

/********** CpuProfiler **********/

CPUProfiler* NewCPUProfiler(IsolatePtr iso_ptr) {
  Isolate* iso = static_cast<Isolate*>(iso_ptr);
  Locker locker(iso);
  Isolate::Scope isolate_scope(iso);
  HandleScope handle_scope(iso);

  CPUProfiler* c = new CPUProfiler;
  c->iso = iso;
  c->ptr = CpuProfiler::New(iso);
  return c;
}

void CPUProfilerDispose(CPUProfiler* profiler) {
  if (profiler->ptr == nullptr) {
    return;
  }
  profiler->ptr->Dispose();

  delete profiler;
}

void CPUProfilerStartProfiling(CPUProfiler* profiler, const char* title) {
  if (profiler->iso == nullptr) {
    return;
  }

  Locker locker(profiler->iso);
  Isolate::Scope isolate_scope(profiler->iso);
  HandleScope handle_scope(profiler->iso);

  Local<String> title_str =
      String::NewFromUtf8(profiler->iso, title, NewStringType::kNormal)
          .ToLocalChecked();
  profiler->ptr->StartProfiling(title_str);
}

CPUProfileNode* NewCPUProfileNode(const CpuProfileNode* ptr_) {
  int count = ptr_->GetChildrenCount();
  CPUProfileNode** children = new CPUProfileNode*[count];
  for (int i = 0; i < count; ++i) {
    children[i] = NewCPUProfileNode(ptr_->GetChild(i));
  }

  CPUProfileNode* root = new CPUProfileNode{
      ptr_,
      ptr_->GetNodeId(),
      ptr_->GetScriptId(),
      ptr_->GetScriptResourceNameStr(),
      ptr_->GetFunctionNameStr(),
      ptr_->GetLineNumber(),
      ptr_->GetColumnNumber(),
      ptr_->GetHitCount(),
      ptr_->GetBailoutReason(),
      count,
      children,
  };
  return root;
}

CPUProfile* CPUProfilerStopProfiling(CPUProfiler* profiler, const char* title) {
  if (profiler->iso == nullptr) {
    return nullptr;
  }

  Locker locker(profiler->iso);
  Isolate::Scope isolate_scope(profiler->iso);
  HandleScope handle_scope(profiler->iso);

  Local<String> title_str =
      String::NewFromUtf8(profiler->iso, title, NewStringType::kNormal)
          .ToLocalChecked();

  CPUProfile* profile = new CPUProfile;
  profile->ptr = profiler->ptr->StopProfiling(title_str);

  Local<String> str = profile->ptr->GetTitle();
  String::Utf8Value t(profiler->iso, str);
  profile->title = CopyString(t);

  CPUProfileNode* root = NewCPUProfileNode(profile->ptr->GetTopDownRoot());
  profile->root = root;

  profile->startTime = profile->ptr->GetStartTime();
  profile->endTime = profile->ptr->GetEndTime();

  return profile;
}

void CPUProfileNodeDelete(CPUProfileNode* node) {
  for (int i = 0; i < node->childrenCount; ++i) {
    CPUProfileNodeDelete(node->children[i]);
  }

  delete[] node->children;
  delete node;
}

void CPUProfileDelete(CPUProfile* profile) {
  if (profile->ptr == nullptr) {
    return;
  }
  profile->ptr->Delete();
  free((void*)profile->title);

  CPUProfileNodeDelete(profile->root);

  delete profile;
}

/********** Template **********/

#define LOCAL_TEMPLATE(tmpl_ptr)     \
  Isolate* iso = tmpl_ptr->iso;      \
  Locker locker(iso);                \
  Isolate::Scope isolate_scope(iso); \
  HandleScope handle_scope(iso);     \
  Local<Template> tmpl = tmpl_ptr->ptr->Get(iso);

void TemplateFreeWrapper(TemplatePtr tmpl) {
  // Intentionally leak the heap-allocated Persistent<Template>. Go's GC may
  // invoke this finalizer after the owning Isolate is already disposed (at
  // program exit, for instance), in which case Reset()ing would trip V8's
  // node->IsInUse() DCHECK. The handle slot is reclaimed when the isolate is
  // destroyed; the memory cost is negligible for templates' typical lifetime.
  delete tmpl;
}

void TemplateRelease(TemplatePtr ptr) {
  if (!ptr) return;
  {
    Isolate* iso = ptr->iso;
    Locker locker(iso);
    Isolate::Scope isolate_scope(iso);
    ptr->ptr->Reset();
  }
  delete ptr->ptr;
  delete ptr;
}

void TemplateSetValue(TemplatePtr ptr,
                      const char* name,
                      ValuePtr val,
                      int attributes) {
  LOCAL_TEMPLATE(ptr);

  Local<String> prop_name =
      String::NewFromUtf8(iso, name, NewStringType::kNormal).ToLocalChecked();
  tmpl->Set(prop_name, val->ptr.Get(iso), (PropertyAttribute)attributes);
}

// A property keyed by the v8::Private named name (Private::ForApi, the one
// Object.SetPrivate and Context.PrivateSymbol name) on every instance the
// template makes: a brand script cannot see, copied in with the instance
// rather than written after it by a second crossing.
void TemplateSetPrivate(TemplatePtr ptr,
                        const char* name,
                        ValuePtr val,
                        int attributes) {
  LOCAL_TEMPLATE(ptr);

  Local<String> prop_name =
      String::NewFromUtf8(iso, name, NewStringType::kNormal).ToLocalChecked();
  tmpl->SetPrivate(Private::ForApi(iso, prop_name), val->ptr.Get(iso),
                   (PropertyAttribute)attributes);
}

void TemplateSetTemplate(TemplatePtr ptr,
                         const char* name,
                         TemplatePtr obj,
                         int attributes) {
  LOCAL_TEMPLATE(ptr);

  Local<String> prop_name =
      String::NewFromUtf8(iso, name, NewStringType::kNormal).ToLocalChecked();
  tmpl->Set(prop_name, obj->ptr->Get(iso), (PropertyAttribute)attributes);
}

/********** ObjectTemplate **********/

TemplatePtr NewObjectTemplate(IsolatePtr iso) {
  Locker locker(iso);
  Isolate::Scope isolate_scope(iso);
  HandleScope handle_scope(iso);

  m_template* ot = new m_template;
  ot->iso = iso;
  ot->ptr = new Persistent<Template>(iso, ObjectTemplate::New(iso));
  return ot;
}

RtnValue ObjectTemplateNewInstance(TemplatePtr ptr, ContextPtr ctx) {
  LOCAL_TEMPLATE(ptr);
  ContextUse ctx_use(ctx);
  TryCatch try_catch(iso);
  Local<Context> local_ctx = ctx->ptr.Get(iso);
  Context::Scope context_scope(local_ctx);

  RtnValue rtn = {};

  Local<ObjectTemplate> obj_tmpl = tmpl.As<ObjectTemplate>();
  Local<Object> obj;
  if (!obj_tmpl->NewInstance(local_ctx).ToLocal(&obj)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }

  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, obj);
  rtn.value = tracked_value(ctx, val);
  return rtn;
}

void ObjectTemplateSetInternalFieldCount(TemplatePtr ptr, int field_count) {
  LOCAL_TEMPLATE(ptr);

  Local<ObjectTemplate> obj_tmpl = tmpl.As<ObjectTemplate>();
  obj_tmpl->SetInternalFieldCount(field_count);
}

int ObjectTemplateInternalFieldCount(TemplatePtr ptr) {
  LOCAL_TEMPLATE(ptr);

  Local<ObjectTemplate> obj_tmpl = tmpl.As<ObjectTemplate>();
  return obj_tmpl->InternalFieldCount();
}

static void FunctionTemplateCallback(const FunctionCallbackInfo<Value>& info);

void ObjectTemplateMarkAsUndetectable(TemplatePtr ptr) {
  LOCAL_TEMPLATE(ptr);

  Local<ObjectTemplate> obj_tmpl = tmpl.As<ObjectTemplate>();
  obj_tmpl->MarkAsUndetectable();
}

void ObjectTemplateSetCallAsFunctionHandler(TemplatePtr ptr, int callback_ref) {
  LOCAL_TEMPLATE(ptr);

  Local<Integer> cbData = Integer::New(iso, callback_ref);
  Local<ObjectTemplate> obj_tmpl = tmpl.As<ObjectTemplate>();
  obj_tmpl->SetCallAsFunctionHandler(FunctionTemplateCallback, cbData);
}

/********** FunctionTemplate **********/

// kNoInternalField must match noInternalField in function_template.go: the
// value that says "the receiver's internal field 0 is not a 32-bit integer",
// chosen outside the int32 range so no real field value collides with it.
static const int64_t kNoInternalField = -(int64_t(1) << 40);

static void FunctionTemplateCallback(const FunctionCallbackInfo<Value>& info) {
  Isolate* iso = info.GetIsolate();
  ISOLATE_SCOPE(iso);

  // This callback function can be called from any Context, which we only know
  // at runtime. We extract the Context reference from the embedder data so that
  // we can use the context registry to match the Context on the Go side
  Local<Context> local_ctx = iso->GetCurrentContext();
  if (local_ctx.IsEmpty()) {
    return;
  }
  Local<Value> ref_val = local_ctx->GetEmbedderData(1);
  if (ref_val.IsEmpty() || !ref_val->IsInt32()) {
    return;
  }
  int ctx_ref = ref_val.As<Integer>()->Value();
  m_ctx* ctx = goContext(ctx_ref);
  if (ctx == nullptr) {
    // The context that owns this function has been closed while something
    // still held the function: a document that has gone away, called from one
    // that has not. V8 enters an API function's OWN creation context before
    // invoking it, so this fires whenever a page keeps a callable — or an
    // object with methods on it — belonging to an iframe it then navigates.
    //
    // goContext answers null for exactly that and says so in its comment, but
    // nothing here checked: tracked_value writes through ctx to
    // ctx->nextValId, sixteen decimal bytes past a null pointer, and the
    // process dies inside cgo where no Go recover can reach it. Five lines of
    // page script were enough —
    //
    //   const d = frame.contentDocument;
    //   frame.onload = () => d.createElement("div");
    //   frame.src = "...";
    //
    // Answering undefined is what the embedder's own guard for this
    // (js.Runtime.gone) already answers when the call gets as far as Go.
    return;
  }
  ContextUse ctx_use(ctx);

  int callback_ref = info.Data().As<Integer>()->Value();

  // Watermark for the return-value release below: any internal-context value
  // id above this was minted by the callback we are about to run.
  m_ctx* internal_ctx = isolateInternalContext(iso);
  long internal_val_watermark = internal_ctx ? internal_ctx->nextValId : 0;

  m_value* _this = new m_value;
  _this->id = 0;
  _this->iso = iso;
  _this->ctx = ctx;
  _this->ptr.Reset(iso, Global<Value>(
                            iso, info.This()));

  int args_count = info.Length();
  ValuePtr thisAndArgs[args_count + 1];
  thisAndArgs[0] = tracked_value(ctx, _this);
  ValuePtr* args = thisAndArgs + 1;
  for (int i = 0; i < args_count; i++) {
    m_value* val = new m_value;
    val->id = 0;
    val->iso = iso;
    val->ctx = ctx;
    val->ptr.Reset(
        iso, Global<Value>(iso, info[i]));
    args[i] = tracked_value(ctx, val);
  }

  // The receiver's internal field 0, read here rather than fetched back: see
  // FunctionCallbackInfo.ThisInternalField. Anything that is not a 32-bit
  // integer — no internal fields at all, an unset field, an embedder storing
  // something else there — reports the sentinel and the Go side falls back to
  // the general path.
  int64_t this_field0 = kNoInternalField;
  {
    Local<Object> self = info.This();
    if (self->InternalFieldCount() > 0) {
      Local<Data> field = self->GetInternalField(0);
      if (!field.IsEmpty() && field->IsValue()) {
        Local<Value> value = field.As<Value>();
        if (value->IsInt32()) {
          this_field0 = value.As<Int32>()->Value();
        }
      }
    }
  }

  ValuePtr val = goFunctionCallback(ctx_ref, callback_ref, thisAndArgs,
                                    args_count, this_field0);
  if (val != nullptr) {
    info.GetReturnValue().Set(val->ptr.Get(iso));

    // Release a return value this call created. Once Set() copies its Local
    // into V8's return slot, V8 roots the underlying value for as long as JS
    // needs it; our m_value wrapper (and the strong Global<Value> it holds)
    // is dead weight from here on. Go callbacks build return values with
    // NewValue(iso, ...), which tracks them in the *isolate's internal*
    // context (iso->GetData(0)->vals) — freed only at IsolateDispose, never
    // at the per-call Context::Close. There is no finalizer on the Go *Value
    // either, so without this drop every callback return value leaks a
    // strongly-reachable pin for the isolate's whole lifetime. A JS Proxy
    // that calls a host function on every property access (see the deskbot
    // msgbridge) turns that into unbounded, un-GC-able heap growth and a
    // "last resort GC frees 0 bytes" CALL_AND_RETRY_LAST OOM.
    //
    // Only values this call minted may be dropped. A returned value the Go
    // side still holds a handle to must survive, and there are three kinds:
    //
    //   - Null(iso) / Undefined(iso), which are per-isolate singletons cached
    //     on the Go *Isolate and returned by callbacks constantly. Freeing one
    //     turns every later Null(iso) into a use-after-free.
    //   - values tracked in a real Context rather than the internal one —
    //     ObjectTemplate instances, RunScript results, callback args. Those
    //     belong to the context and are freed at its Close.
    //   - anything created before this call and cached on the Go side, such as
    //     a DOM binding's per-node wrapper object returned on every access.
    //
    // The watermark taken before the callback ran separates them: an internal
    // context id above it can only have come from a NewValue(iso, ...) inside
    // this call, which is exactly the leak the drop exists to prevent.
    bool mintedByThisCall = internal_ctx != nullptr && val->ctx == internal_ctx &&
                            val->id > internal_val_watermark;

    // `this` and the args are context-tracked, so mintedByThisCall already
    // excludes them; the identity check is belt and braces against a future
    // change to how they are tracked, where a double-free would be the cost.
    bool aliasesThisOrArg = false;
    for (int i = 0; i <= args_count; i++) {
      if (thisAndArgs[i] == val) {
        aliasesThisOrArg = true;
        break;
      }
    }
    if (mintedByThisCall && !aliasesThisOrArg) {
      if (val->id != 0 && val->ctx != nullptr) {
        val->ctx->vals.erase(val->id);
      }
      val->ptr.Reset();
      delete val;
    }
  } else {
    info.GetReturnValue().SetUndefined();
  }
}

TemplatePtr NewFunctionTemplate(IsolatePtr iso, int callback_ref) {
  Locker locker(iso);
  Isolate::Scope isolate_scope(iso);
  HandleScope handle_scope(iso);

  // (rogchap) We only need to store one value, callback_ref, into the
  // C++ callback function data, but if we needed to store more items we could
  // use an V8::Array; this would require the internal context from
  // iso->GetData(0)
  Local<Integer> cbData = Integer::New(iso, callback_ref);

  m_template* ot = new m_template;
  ot->iso = iso;
  ot->ptr = new Persistent<Template>(
      iso, FunctionTemplate::New(iso, FunctionTemplateCallback, cbData));
  return ot;
}

RtnValue FunctionTemplateGetFunction(TemplatePtr ptr, ContextPtr ctx) {
  LOCAL_TEMPLATE(ptr);
  ContextUse ctx_use(ctx);
  TryCatch try_catch(iso);
  Local<Context> local_ctx = ctx->ptr.Get(iso);
  Context::Scope context_scope(local_ctx);

  Local<FunctionTemplate> fn_tmpl = tmpl.As<FunctionTemplate>();
  RtnValue rtn = {};
  Local<Function> fn;
  if (!fn_tmpl->GetFunction(local_ctx).ToLocal(&fn)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }

  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, fn);
  rtn.value = tracked_value(ctx, val);
  return rtn;
}

void FunctionTemplatePrototypeSetValue(TemplatePtr ptr,
                                       const char* name,
                                       ValuePtr val,
                                       int attributes) {
  LOCAL_TEMPLATE(ptr);

  Local<FunctionTemplate> fn_tmpl = tmpl.As<FunctionTemplate>();
  Local<ObjectTemplate> proto = fn_tmpl->PrototypeTemplate();
  Local<String> prop_name =
      String::NewFromUtf8(iso, name, NewStringType::kNormal).ToLocalChecked();
  proto->Set(prop_name, val->ptr.Get(iso), (PropertyAttribute)attributes);
}

TemplatePtr FunctionTemplatePrototypeSetMethod(TemplatePtr ptr,
                                               const char* name,
                                               int callback_ref) {
  LOCAL_TEMPLATE(ptr);

  Local<FunctionTemplate> parent = tmpl.As<FunctionTemplate>();
  Local<ObjectTemplate> proto = parent->PrototypeTemplate();
  Local<String> prop_name =
      String::NewFromUtf8(iso, name, NewStringType::kNormal).ToLocalChecked();

  Local<Integer> cbData = Integer::New(iso, callback_ref);
  Local<FunctionTemplate> child =
      FunctionTemplate::New(iso, FunctionTemplateCallback, cbData);
  proto->Set(prop_name, child);

  m_template* ot = new m_template;
  ot->iso = iso;
  ot->ptr = new Persistent<Template>(iso, child);
  return ot;
}

/********** Fast callbacks **********/

static void weakValueCallback(const WeakCallbackInfo<m_value>& info);
static void weakValueReleasingCallback(const WeakCallbackInfo<m_value>& info);
static void weakTokenCallback(const WeakCallbackInfo<m_value>& info);

// The template's data is one Number: the callback reference times 2^16 plus
// the spec, two bits an argument ('v' = 1, 's' = 2), so a call reads both
// with one load. A double holds a reference up to 2^37, and a reference is a
// per-isolate counter of templates made.
static const double kFastSpecRadix = 65536.0;

static int fastSpecBits(const char* spec) {
  int bits = 0;
  for (int i = 0; spec != nullptr && spec[i] != 0 && i < kFastMaxArgs; i++) {
    int code = spec[i] == 's' ? 2 : 1;
    bits |= code << (2 * i);
  }
  return bits;
}

// fastDecode fills |out| for one argument. |storage| owns a string's bytes
// for the rest of the call. False means a conversion threw: the exception is
// pending and the call must return without running the binding.
static bool fastDecode(Isolate* iso,
                       Local<Context> local_ctx,
                       Local<Value> v,
                       int code,
                       FastArg* out,
                       std::string* storage) {
  out->i32 = 0;
  out->f64 = 0;
  out->str = nullptr;
  out->len = 0;
  if (v->IsUndefined()) {
    out->kind = FastUndefined;
  } else if (v->IsNull()) {
    out->kind = FastNull;
  } else if (v->IsBoolean()) {
    out->kind = FastBool;
    out->i32 = v->IsTrue() ? 1 : 0;
  } else if (v->IsInt32()) {
    out->kind = FastInt32;
    out->i32 = v.As<Int32>()->Value();
    out->f64 = out->i32;
  } else if (v->IsNumber()) {
    out->kind = FastNumber;
    out->f64 = v.As<Number>()->Value();
  } else if (v->IsString()) {
    out->kind = FastString;
  } else if (v->IsObject()) {
    out->kind = FastObject;
    Local<Object> obj = v.As<Object>();
    if (obj->InternalFieldCount() > 0) {
      Local<Data> field = obj->GetInternalField(0);
      if (!field.IsEmpty() && field->IsValue() &&
          field.As<Value>()->IsInt32()) {
        out->kind = FastWrapper;
        out->i32 = field.As<Value>().As<Int32>()->Value();
      }
    }
  } else {
    out->kind = FastOther;
  }
  Local<String> str;
  if (out->kind == FastString) {
    str = v.As<String>();
  } else if (code == 2) {
    if (!v->ToString(local_ctx).ToLocal(&str)) {
      return false;
    }
  } else {
    return true;
  }
  size_t len = str->Utf8LengthV2(iso);
  storage->resize(len);
  str->WriteUtf8V2(iso, storage->data(), len);
  out->str = storage->data();
  out->len = static_cast<int>(len);
  return true;
}

// The per-isolate buffer a string result is written into. 64 KiB: a longer
// result comes back as a minted value instead.
static const int kFastRetBufSize = 64 * 1024;
static thread_local char* fastRetBuf = nullptr;

static void FastFunctionTemplateCallback(
    const FunctionCallbackInfo<Value>& info) {
  Isolate* iso = info.GetIsolate();
  HandleScope handle_scope(iso);
  Local<Context> local_ctx = iso->GetCurrentContext();
  if (local_ctx.IsEmpty()) {
    return;
  }
  Local<Value> ref_val = local_ctx->GetEmbedderData(1);
  if (ref_val.IsEmpty() || !ref_val->IsInt32()) {
    return;
  }
  int ctx_ref = ref_val.As<Integer>()->Value();

  double data = info.Data().As<Number>()->Value();
  int callback_ref = static_cast<int>(data / kFastSpecRadix);
  int spec = static_cast<int>(data - callback_ref * kFastSpecRadix);

  int64_t this_field0 = kNoInternalField;
  {
    Local<Object> self = info.This();
    if (self->InternalFieldCount() > 0) {
      Local<Data> field = self->GetInternalField(0);
      if (!field.IsEmpty() && field->IsValue()) {
        Local<Value> value = field.As<Value>();
        if (value->IsInt32()) {
          this_field0 = value.As<Int32>()->Value();
        }
      }
    }
  }

  int args_count = info.Length();
  FastArg args[kFastMaxArgs];
  std::string storage[kFastMaxArgs];
  for (int i = 0; i < kFastMaxArgs; i++) {
    int code = (spec >> (2 * i)) & 3;
    if (code == 0) {
      break;
    }
    if (i >= args_count) {
      args[i].kind = FastAbsent;
      args[i].i32 = 0;
      args[i].f64 = 0;
      args[i].str = nullptr;
      args[i].len = 0;
      continue;
    }
    if (!fastDecode(iso, local_ctx, info[i], code, &args[i], &storage[i])) {
      return;
    }
  }

  if (fastRetBuf == nullptr) {
    fastRetBuf = static_cast<char*>(malloc(kFastRetBufSize));
  }
  FastRet ret = {};
  ret.kind = FastRetUndefined;
  ret.buf = fastRetBuf;
  ret.cap = kFastRetBufSize;

  goFastCallback(ctx_ref, callback_ref, this_field0, args_count, args, &ret);

  ReturnValue<Value> rv = info.GetReturnValue();
  switch (ret.kind) {
    case FastRetNull:
      rv.SetNull();
      break;
    case FastRetBool:
      rv.Set(ret.i32 != 0);
      break;
    case FastRetInt32:
      rv.Set(ret.i32);
      break;
    case FastRetNumber:
      rv.Set(ret.f64);
      break;
    case FastRetString: {
      Local<String> str;
      if (String::NewFromUtf8(iso, ret.buf, NewStringType::kNormal, ret.len)
              .ToLocal(&str)) {
        rv.Set(str);
      }
      break;
    }
    case FastRetValue:
      if (ret.value != nullptr) {
        rv.Set(ret.value->ptr.Get(iso));
      }
      break;
    case FastRetOwnedValue:
      if (ret.value != nullptr) {
        m_value* val = ret.value;
        rv.Set(val->ptr.Get(iso));
        if (val->id != 0 && val->ctx != nullptr) {
          val->ctx->vals.erase(val->id);
        }
        val->ptr.Reset();
        delete val;
      }
      break;
    default:
      rv.SetUndefined();
  }
}

TemplatePtr NewFastFunctionTemplate(IsolatePtr iso,
                                    int callback_ref,
                                    const char* spec) {
  Locker locker(iso);
  Isolate::Scope isolate_scope(iso);
  HandleScope handle_scope(iso);

  Local<Number> cbData = Number::New(
      iso, callback_ref * kFastSpecRadix + fastSpecBits(spec));
  m_template* ot = new m_template;
  ot->iso = iso;
  ot->ptr = new Persistent<Template>(
      iso, FunctionTemplate::New(iso, FastFunctionTemplateCallback, cbData));
  return ot;
}

RtnValue ObjectTemplateNewWrapper(TemplatePtr ptr,
                                  ContextPtr ctx,
                                  int32_t field,
                                  ValuePtr proto,
                                  int n,
                                  ValuePtr* keys,
                                  ValuePtr* vals,
                                  int64_t weak_token) {
  LOCAL_TEMPLATE(ptr);
  ContextUse ctx_use(ctx);
  TryCatch try_catch(iso);
  Local<Context> local_ctx = ctx->ptr.Get(iso);
  Context::Scope context_scope(local_ctx);

  RtnValue rtn = {};
  Local<ObjectTemplate> obj_tmpl = tmpl.As<ObjectTemplate>();
  Local<Object> obj;
  if (!obj_tmpl->NewInstance(local_ctx).ToLocal(&obj)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  if (obj->InternalFieldCount() > 0) {
    obj->SetInternalField(0, Integer::New(iso, field));
  }
  if (proto != nullptr) {
    Local<Value> p = proto->ptr.Get(iso);
    if (!obj->SetPrototype(local_ctx, p).FromMaybe(false)) {
      rtn.error = ExceptionError(try_catch, iso, local_ctx);
      return rtn;
    }
  }
  for (int i = 0; i < n; i++) {
    if (keys[i] == nullptr || vals[i] == nullptr) {
      continue;
    }
    if (obj->Set(local_ctx, keys[i]->ptr.Get(iso), vals[i]->ptr.Get(iso))
            .IsNothing()) {
      rtn.error = ExceptionError(try_catch, iso, local_ctx);
      return rtn;
    }
  }

  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, obj);
  rtn.value = tracked_value(ctx, val);
  if (weak_token != 0) {
    val->weak_token = weak_token;
    val->ptr.SetWeak(val, weakTokenCallback, WeakCallbackType::kParameter);
  }
  return rtn;
}

RtnValue NewArrayOfValues(ContextPtr ctx, int n, ValuePtr* vals) {
  Isolate* iso = ctx->iso;
  Locker locker(iso);
  Isolate::Scope isolate_scope(iso);
  ContextUse ctx_use(ctx);
  HandleScope handle_scope(iso);
  Local<Context> local_ctx = ctx->ptr.Get(iso);
  Context::Scope context_scope(local_ctx);
  RtnValue rtn = {};
  std::vector<Local<Value>> elements(n);
  for (int i = 0; i < n; i++) {
    if (vals[i] == nullptr) {
      elements[i] = Undefined(iso);
    } else {
      elements[i] = vals[i]->ptr.Get(iso);
    }
  }
  Local<Array> arr = Array::New(iso, elements.data(), n);
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, arr);
  rtn.value = tracked_value(ctx, val);
  return rtn;
}

// weakValueReleasingCallback is weakValueCallback for a handle the Go side
// handed over entirely: once Go has heard of the collection, the handle is
// freed here rather than by a Release crossing back.
static void weakValueReleasingCallback(const WeakCallbackInfo<m_value>& info) {
  m_value* val = info.GetParameter();
  if (val == nullptr) {
    return;
  }
  val->ptr.Reset();
  goWeakCallback(static_cast<void*>(val));
  if (val->id != 0 && val->ctx != nullptr) {
    val->ctx->vals.erase(val->id);
  }
  delete val;
}

// weakTokenCallback frees a token-weak handle and reports its token to the
// process's token handler: no Go-side registry per handle, and no closure.
static void weakTokenCallback(const WeakCallbackInfo<m_value>& info) {
  m_value* val = info.GetParameter();
  if (val == nullptr) {
    return;
  }
  int64_t token = val->weak_token;
  val->ptr.Reset();
  if (val->id != 0 && val->ctx != nullptr) {
    val->ctx->vals.erase(val->id);
  }
  delete val;
  goWeakTokenCallback(token);
}

void ValueSetWeakToken(ValuePtr ptr, int64_t token) {
  if (ptr == nullptr || ptr->ptr.IsEmpty()) {
    return;
  }
  ptr->weak_token = token;
  ptr->ptr.SetWeak(ptr, weakTokenCallback, WeakCallbackType::kParameter);
}

void ValueSetWeakReleasing(ValuePtr ptr) {
  if (ptr == nullptr || ptr->ptr.IsEmpty()) {
    return;
  }
  ptr->ptr.SetWeak(ptr, weakValueReleasingCallback,
                   WeakCallbackType::kParameter);
}

void ValuesSetWeak(int n, ValuePtr* ptrs, int releasing) {
  for (int i = 0; i < n; i++) {
    ValuePtr ptr = ptrs[i];
    if (ptr == nullptr || ptr->ptr.IsEmpty()) {
      continue;
    }
    if (releasing) {
      ptr->ptr.SetWeak(ptr, weakValueReleasingCallback,
                       WeakCallbackType::kParameter);
    } else {
      ptr->ptr.SetWeak(ptr, weakValueCallback, WeakCallbackType::kParameter);
    }
  }
}

/********** WebIDL binding shape **********/

// A generated interface is a FunctionTemplate whose prototype carries accessor
// pairs and methods, whose instances carry internal fields, and which inherits
// from its parent interface's template. What follows is what that needs and
// what v8go did not expose.

void FunctionTemplateInherit(TemplatePtr ptr, TemplatePtr parent) {
  LOCAL_TEMPLATE(ptr);
  Local<Template> parent_tmpl = parent->ptr->Get(iso);
  tmpl.As<FunctionTemplate>()->Inherit(parent_tmpl.As<FunctionTemplate>());
}

static TemplatePtr wrapTemplate(Isolate* iso, Local<Template> child) {
  m_template* ot = new m_template;
  ot->iso = iso;
  ot->ptr = new Persistent<Template>(iso, child);
  return ot;
}

TemplatePtr FunctionTemplateInstanceTemplate(TemplatePtr ptr) {
  LOCAL_TEMPLATE(ptr);
  return wrapTemplate(iso, tmpl.As<FunctionTemplate>()->InstanceTemplate());
}

TemplatePtr FunctionTemplatePrototypeTemplate(TemplatePtr ptr) {
  LOCAL_TEMPLATE(ptr);
  return wrapTemplate(iso, tmpl.As<FunctionTemplate>()->PrototypeTemplate());
}

void FunctionTemplateSetClassName(TemplatePtr ptr, const char* name) {
  LOCAL_TEMPLATE(ptr);
  Local<String> class_name =
      String::NewFromUtf8(iso, name, NewStringType::kNormal).ToLocalChecked();
  tmpl.As<FunctionTemplate>()->SetClassName(class_name);
}

void FunctionTemplateSetLength(TemplatePtr ptr, int length) {
  LOCAL_TEMPLATE(ptr);
  tmpl.As<FunctionTemplate>()->SetLength(length);
}

void FunctionTemplateReadOnlyPrototype(TemplatePtr ptr) {
  LOCAL_TEMPLATE(ptr);
  tmpl.As<FunctionTemplate>()->ReadOnlyPrototype();
}

void TemplateSetAccessorProperty(TemplatePtr ptr,
                                 const char* name,
                                 int getter_ref,
                                 int setter_ref,
                                 int attributes) {
  LOCAL_TEMPLATE(ptr);
  Local<String> prop_name =
      String::NewFromUtf8(iso, name, NewStringType::kNormal).ToLocalChecked();

  Local<FunctionTemplate> getter;
  if (getter_ref >= 0) {
    getter = FunctionTemplate::New(iso, FunctionTemplateCallback,
                                   Integer::New(iso, getter_ref));
  }
  Local<FunctionTemplate> setter;
  if (setter_ref >= 0) {
    setter = FunctionTemplate::New(iso, FunctionTemplateCallback,
                                   Integer::New(iso, setter_ref));
  }
  tmpl->SetAccessorProperty(prop_name, getter, setter,
                            static_cast<PropertyAttribute>(attributes));
}

static Local<Symbol> wellKnownSymbol(Isolate* iso, const char* symbol) {
  std::string name(symbol);
  if (name == "toStringTag") {
    return Symbol::GetToStringTag(iso);
  }
  if (name == "iterator") {
    return Symbol::GetIterator(iso);
  }
  if (name == "asyncIterator") {
    return Symbol::GetAsyncIterator(iso);
  }
  if (name == "hasInstance") {
    return Symbol::GetHasInstance(iso);
  }
  if (name == "toPrimitive") {
    return Symbol::GetToPrimitive(iso);
  }
  if (name == "unscopables") {
    return Symbol::GetUnscopables(iso);
  }
  return Local<Symbol>();
}

void TemplateSetSymbolValue(TemplatePtr ptr,
                            const char* symbol,
                            ValuePtr val,
                            int attributes) {
  LOCAL_TEMPLATE(ptr);
  Local<Symbol> key = wellKnownSymbol(iso, symbol);
  if (key.IsEmpty()) {
    return;
  }
  Local<Value> value = val->ptr.Get(iso);
  tmpl->Set(key, value, static_cast<PropertyAttribute>(attributes));
}

TemplatePtr TemplateSetSymbolMethod(TemplatePtr ptr,
                                    const char* symbol,
                                    int callback_ref) {
  LOCAL_TEMPLATE(ptr);
  Local<Symbol> key = wellKnownSymbol(iso, symbol);
  if (key.IsEmpty()) {
    return nullptr;
  }
  Local<FunctionTemplate> child = FunctionTemplate::New(
      iso, FunctionTemplateCallback, Integer::New(iso, callback_ref));
  tmpl->Set(key, child);
  return wrapTemplate(iso, child);
}

/********** Property interceptors **********/

// The DOM is full of objects whose property names are not known in advance:
// document.forms.myForm, collection[3], localStorage.token, el.dataset.userId.
// Defining each name eagerly is slower than intercepting and observably wrong
// — a name that appears after the object was built is simply absent.
//
// One Go callback per operation, dispatched through the same registry
// FunctionTemplateCallback uses. The Go side receives the property name as the
// single argument (or the index as a number) and the receiver as `this`, so an
// embedder writes an interceptor the way it writes any other callback.

namespace {

// interceptorRefs is the set of callback refs one handler was built from,
// stored in the template's data so the trampolines can find them. V8 gives
// each callback its own Data slot, so each is given just its own ref.
struct InterceptorCall {
  Isolate* iso;
  m_ctx* ctx;
  int ctx_ref;
  int callback_ref;
};

bool interceptorContext(const PropertyCallbackInfo<Value>& info,
                        int* ctx_ref,
                        m_ctx** ctx,
                        int* callback_ref) {
  Isolate* iso = info.GetIsolate();
  Local<Context> local_ctx = iso->GetCurrentContext();
  if (local_ctx.IsEmpty()) {
    return false;
  }
  Local<Value> ref_val = local_ctx->GetEmbedderData(1);
  if (ref_val.IsEmpty() || !ref_val->IsInt32()) {
    return false;
  }
  *ctx_ref = ref_val.As<Integer>()->Value();
  *ctx = goContext(*ctx_ref);
  // A closed context: the interceptor declines rather than dereferencing null
  // in callInterceptor's tracked_value. See FunctionTemplateCallback.
  if (*ctx == nullptr) {
    return false;
  }
  *callback_ref = info.Data().As<Integer>()->Value();
  return true;
}

// callInterceptor invokes the Go callback with |key| as its only argument and
// the receiver as `this`, and answers the returned value (nullptr when the Go
// side declined).
ValuePtr callInterceptor(Isolate* iso,
                         Local<Object> self,
                         Local<Value> key,
                         int ctx_ref,
                         m_ctx* ctx,
                         int callback_ref) {
  ContextUse ctx_use(ctx);
  m_value* _this = new m_value;
  _this->id = 0;
  _this->iso = iso;
  _this->ctx = ctx;
  _this->ptr.Reset(iso, Global<Value>(iso, self));

  ValuePtr thisAndArgs[2];
  thisAndArgs[0] = tracked_value(ctx, _this);

  m_value* arg = new m_value;
  arg->id = 0;
  arg->iso = iso;
  arg->ctx = ctx;
  arg->ptr.Reset(iso, Global<Value>(iso, key));
  thisAndArgs[1] = tracked_value(ctx, arg);

  int64_t this_field0 = kNoInternalField;
  if (self->InternalFieldCount() > 0) {
    Local<Data> field = self->GetInternalField(0);
    if (!field.IsEmpty() && field->IsValue()) {
      Local<Value> value = field.As<Value>();
      if (value->IsInt32()) {
        this_field0 = value.As<Int32>()->Value();
      }
    }
  }
  return goFunctionCallback(ctx_ref, callback_ref, thisAndArgs, 1, this_field0);
}

// The getters answer Intercepted::kYes when they handled the request and kNo
// when V8 should keep looking — the object's own properties, then its
// prototype chain. Answering kYes with undefined would make every unknown name
// on a collection resolve to undefined and stop the lookup, which is how
// `document.forms.constructor` would come back undefined.
Intercepted namedGetter(Local<Name> name,
                        const PropertyCallbackInfo<Value>& info) {
  int ctx_ref, callback_ref;
  m_ctx* ctx;
  if (!interceptorContext(info, &ctx_ref, &ctx, &callback_ref)) {
    return Intercepted::kNo;
  }
  Isolate* iso = info.GetIsolate();
  ValuePtr val =
      callInterceptor(iso, info.Holder(), name, ctx_ref, ctx, callback_ref);
  if (val == nullptr) {
    return Intercepted::kNo;
  }
  Local<Value> result = val->ptr.Get(iso);
  if (result->IsUndefined()) {
    return Intercepted::kNo;  // declined, expressed as undefined
  }
  info.GetReturnValue().Set(result);
  return Intercepted::kYes;
}

Intercepted indexedGetter(uint32_t index,
                          const PropertyCallbackInfo<Value>& info) {
  int ctx_ref, callback_ref;
  m_ctx* ctx;
  if (!interceptorContext(info, &ctx_ref, &ctx, &callback_ref)) {
    return Intercepted::kNo;
  }
  Isolate* iso = info.GetIsolate();
  Local<Value> key = Integer::NewFromUnsigned(iso, index);
  ValuePtr val =
      callInterceptor(iso, info.Holder(), key, ctx_ref, ctx, callback_ref);
  if (val == nullptr) {
    return Intercepted::kNo;
  }
  Local<Value> result = val->ptr.Get(iso);
  if (result->IsUndefined()) {
    return Intercepted::kNo;
  }
  info.GetReturnValue().Set(result);
  return Intercepted::kYes;
}

void namedEnumerator(const PropertyCallbackInfo<Array>& info) {
  Isolate* iso = info.GetIsolate();
  Local<Context> local_ctx = iso->GetCurrentContext();
  if (local_ctx.IsEmpty()) {
    return;
  }
  Local<Value> ref_val = local_ctx->GetEmbedderData(1);
  if (ref_val.IsEmpty() || !ref_val->IsInt32()) {
    return;
  }
  int ctx_ref = ref_val.As<Integer>()->Value();
  m_ctx* ctx = goContext(ctx_ref);
  // A closed context enumerates nothing rather than dereferencing null. See
  // FunctionTemplateCallback.
  if (ctx == nullptr) {
    return;
  }
  ContextUse ctx_use(ctx);
  int callback_ref = info.Data().As<Integer>()->Value();

  m_value* _this = new m_value;
  _this->id = 0;
  _this->iso = iso;
  _this->ctx = ctx;
  _this->ptr.Reset(iso, Global<Value>(iso, info.Holder()));
  ValuePtr thisAndArgs[1];
  thisAndArgs[0] = tracked_value(ctx, _this);

  ValuePtr val = goFunctionCallback(ctx_ref, callback_ref, thisAndArgs, 0,
                                    kNoInternalField);
  if (val == nullptr) {
    return;
  }
  Local<Value> result = val->ptr.Get(iso);
  if (result->IsArray()) {
    info.GetReturnValue().Set(result.As<Array>());
  }
}

PropertyHandlerFlags handlerFlags(int flags) {
  PropertyHandlerFlags out = PropertyHandlerFlags::kNone;
  if (flags & 1) {
    out = static_cast<PropertyHandlerFlags>(static_cast<int>(out) |
                                            static_cast<int>(PropertyHandlerFlags::kNonMasking));
  }
  if (flags & 2) {
    out = static_cast<PropertyHandlerFlags>(
        static_cast<int>(out) |
        static_cast<int>(PropertyHandlerFlags::kOnlyInterceptStrings));
  }
  if (flags & 4) {
    out = static_cast<PropertyHandlerFlags>(
        static_cast<int>(out) |
        static_cast<int>(PropertyHandlerFlags::kHasNoSideEffect));
  }
  return out;
}

}  // namespace

void ObjectTemplateSetNamedPropertyHandler(TemplatePtr ptr,
                                           int getter_ref,
                                           int setter_ref,
                                           int query_ref,
                                           int deleter_ref,
                                           int enumerator_ref,
                                           int flags) {
  LOCAL_TEMPLATE(ptr);
  Local<ObjectTemplate> obj_tmpl = tmpl.As<ObjectTemplate>();

  // Only the getter and the enumerator are wired. A setter, query or deleter
  // needs a different signature each, and nothing in this engine's DOM needs
  // one: a named-access object answers reads and enumerates, and writes fall
  // through to the object itself, which is what the specification says for
  // every one of them. Adding one later is mechanical; guessing at it now
  // would be an untested code path with no caller.
  (void)setter_ref;
  (void)query_ref;
  (void)deleter_ref;

  if (getter_ref < 0) {
    return;
  }
  NamedPropertyHandlerConfiguration config(
      namedGetter, nullptr, nullptr, nullptr,
      enumerator_ref >= 0 ? namedEnumerator : nullptr,
      Integer::New(iso, getter_ref), handlerFlags(flags));
  obj_tmpl->SetHandler(config);
}

void ObjectTemplateSetIndexedPropertyHandler(TemplatePtr ptr,
                                             int getter_ref,
                                             int setter_ref,
                                             int query_ref,
                                             int deleter_ref,
                                             int enumerator_ref,
                                             int flags) {
  LOCAL_TEMPLATE(ptr);
  Local<ObjectTemplate> obj_tmpl = tmpl.As<ObjectTemplate>();
  (void)setter_ref;
  (void)query_ref;
  (void)deleter_ref;
  (void)enumerator_ref;

  if (getter_ref < 0) {
    return;
  }
  IndexedPropertyHandlerConfiguration config(
      indexedGetter, nullptr, nullptr, nullptr, nullptr,
      Integer::New(iso, getter_ref), handlerFlags(flags));
  obj_tmpl->SetHandler(config);
}

/********** Context **********/

#define LOCAL_CONTEXT(ctx)                      \
  Isolate* iso = ctx->iso;                      \
  Locker locker(iso);                           \
  Isolate::Scope isolate_scope(iso);            \
  ContextUse ctx_use(ctx);                      \
  HandleScope handle_scope(iso);                \
  TryCatch try_catch(iso);                      \
  Local<Context> local_ctx = ctx->ptr.Get(iso); \
  Context::Scope context_scope(local_ctx);

// The global proxies parked between a context's teardown and its successor's
// creation, keyed by the outgoing context's own reference number. Not by
// isolate: a tab and every frame in it share one, and an isolate-wide slot let
// a frame adopt the page's window.
// ---- embedder extensions --------------------------------------------------

void RegisterExtensionSource(const char* name,
                             const char* source,
                             int source_len,
                             const char** deps,
                             int dep_count) {
  // V8 keeps the extension, and the strings it was built from, for the life
  // of the process: copied here and never freed on purpose.
  char* name_copy = strdup(name);
  char* source_copy = static_cast<char*>(malloc(source_len + 1));
  memcpy(source_copy, source, source_len);
  source_copy[source_len] = 0;
  const char** deps_copy = nullptr;
  if (dep_count > 0) {
    deps_copy = static_cast<const char**>(malloc(sizeof(char*) * dep_count));
    for (int i = 0; i < dep_count; i++) {
      deps_copy[i] = strdup(deps[i]);
    }
  }
  v8::RegisterExtension(std::make_unique<v8::Extension>(
      name_copy, source_copy, dep_count, deps_copy, source_len));
}

// The extensions each isolate's new contexts install. The names are owned
// here; the pointer vector is what ExtensionConfiguration reads.
struct contextExtensions {
  std::vector<std::string> names;
  std::vector<const char*> ptrs;
};

static std::unordered_map<Isolate*, contextExtensions>& isolateExtensions() {
  static auto* m = new std::unordered_map<Isolate*, contextExtensions>();
  return *m;
}

void IsolateSetContextExtensions(IsolatePtr iso, const char** names, int count) {
  auto& all = isolateExtensions();
  if (count <= 0) {
    all.erase(iso);
    return;
  }
  contextExtensions& ext = all[iso];
  ext.names.assign(names, names + count);
  ext.ptrs.clear();
  for (auto& n : ext.names) {
    ext.ptrs.push_back(n.c_str());
  }
}

// newContextWithExtensions is Context::New with the isolate's configured
// extensions, when it has any.
static Local<Context> newContextWithExtensions(
    Isolate* iso,
    Local<ObjectTemplate> global_template,
    MaybeLocal<Value> global_object) {
  auto& all = isolateExtensions();
  auto it = all.find(iso);
  if (it == all.end() || it->second.ptrs.empty()) {
    return Context::New(iso, nullptr, global_template, global_object);
  }
  ExtensionConfiguration config(static_cast<int>(it->second.ptrs.size()),
                                it->second.ptrs.data());
  return Context::New(iso, &config, global_template, global_object);
}

static std::unordered_map<int, Global<Value>>& keptGlobals() {
  static std::unordered_map<int, Global<Value>> kept;
  return kept;
}

int ContextKeepGlobal(ContextPtr ctx, int ref) {
  if (ctx == nullptr || ref == 0) {
    return 0;
  }
  Isolate* iso = ctx->iso;
  Locker locker(iso);
  Isolate::Scope isolate_scope(iso);
  HandleScope handle_scope(iso);
  Local<Context> local_ctx = ctx->ptr.Get(iso);
  if (local_ctx.IsEmpty()) {
    return 0;
  }
  Local<Object> proxy = local_ctx->Global();
  if (proxy.IsEmpty()) {
    return 0;
  }
  local_ctx->DetachGlobal();
  keptGlobals()[ref].Reset(iso, proxy);
  return 1;
}

ContextPtr NewContextAdoptingGlobal(IsolatePtr iso,
                                    TemplatePtr global_template_ptr,
                                    int ref,
                                    int adopt_from) {
  Locker locker(iso);
  Isolate::Scope isolate_scope(iso);
  HandleScope handle_scope(iso);

  Local<ObjectTemplate> global_template;
  if (global_template_ptr != nullptr) {
    global_template = global_template_ptr->ptr->Get(iso).As<ObjectTemplate>();
  } else {
    global_template = ObjectTemplate::New(iso);
  }

  MaybeLocal<Value> global_object;
  auto& kept = keptGlobals();
  auto it = kept.find(adopt_from);
  if (it != kept.end()) {
    if (!it->second.IsEmpty()) {
      global_object = MaybeLocal<Value>(it->second.Get(iso));
    }
    it->second.Reset();
    kept.erase(it);
  }

  Local<Context> local_ctx =
      newContextWithExtensions(iso, global_template, global_object);
  local_ctx->SetEmbedderData(1, Integer::New(iso, ref));

  m_ctx* ctx = new m_ctx;
  ctx->ptr.Reset(iso, local_ctx);
  ctx->iso = iso;
  return ctx;
}

ContextPtr NewContext(IsolatePtr iso,
                      TemplatePtr global_template_ptr,
                      int ref) {
  Locker locker(iso);
  Isolate::Scope isolate_scope(iso);
  HandleScope handle_scope(iso);

  Local<ObjectTemplate> global_template;
  if (global_template_ptr != nullptr) {
    global_template = global_template_ptr->ptr->Get(iso).As<ObjectTemplate>();
  } else {
    global_template = ObjectTemplate::New(iso);
  }

  // For function callbacks we need a reference to the context, but because of
  // the complexities of C -> Go function pointers, we store a reference to the
  // context as a simple integer identifier; this can then be used on the Go
  // side to lookup the context in the context registry. We use slot 1 as slot 0
  // has special meaning for the Chrome debugger.
  Local<Context> local_ctx =
      newContextWithExtensions(iso, global_template, MaybeLocal<Value>());
  local_ctx->SetEmbedderData(1, Integer::New(iso, ref));

  m_ctx* ctx = new m_ctx;
  ctx->ptr.Reset(iso, local_ctx);
  ctx->iso = iso;
  return ctx;
}

// IsolateInternalContextValueCount returns the number of m_value wrappers
// tracked against the isolate's internal context (iso->GetData(0)). Values
// created via NewValue(iso, ...) — including those a FunctionTemplate callback
// builds for its return value — land here and live until IsolateDispose.
// Test-only observability for the callback-return-value leak regression.
int IsolateInternalContextValueCount(IsolatePtr iso) {
  return isolateInternalContext(iso)->vals.size();
}

int ContextRetainedValueCount(ContextPtr ctx) {
  return ctx->vals.size();
}

// ContextInUse reports whether a C frame on the stack is working in the
// context — an entry into it, or a Go callback it dispatched. Freeing it then
// would leave that frame writing through a deleted m_ctx and releasing deleted
// values on its way out; an embedder that tears contexts down from inside
// script (a frame whose element was removed) asks this first.
int ContextInUse(ContextPtr ctx) {
  return ctx != nullptr && ctx->use_depth > 0;
}

void ContextFree(ContextPtr ctx) {
  if (ctx == nullptr) {
    return;
  }
  ctx->ptr.Reset();

  for (auto it = ctx->vals.begin(); it != ctx->vals.end(); ++it) {
    auto value = it->second;
    value->ptr.Reset();
    delete value;
  }
  ctx->vals.clear();

  for (m_unboundScript* us : ctx->unboundScripts) {
    us->ptr.Reset();
    delete us;
  }

  for (auto& record : ctx->moduleRecords) {
    record.second.Reset();
  }
  ctx->moduleRecords.clear();

  // Freed from inside a frame still working in it: that frame writes through
  // the struct on its way out, so it outlives the free until the frame leaves.
  if (ctx->use_depth > 0) {
    ctx->freed = true;
    return;
  }
  contextDelete(ctx);
}


RtnValue RunScript(ContextPtr ctx,
                   const char* source,
                   int source_len,
                   const char* origin) {
  LOCAL_CONTEXT(ctx);

  RtnValue rtn = {};

  // By length: a script's text may hold U+0000, which a C string would end
  // at — and the script would then fail to parse, or run truncated.
  MaybeLocal<String> maybeSrc =
      String::NewFromUtf8(iso, source, NewStringType::kNormal, source_len);
  MaybeLocal<String> maybeOgn =
      String::NewFromUtf8(iso, origin, NewStringType::kNormal);
  Local<String> src, ogn;
  if (!maybeSrc.ToLocal(&src) || !maybeOgn.ToLocal(&ogn)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }

  ScriptOrigin script_origin(ogn);
  Local<Script> script;
  if (!Script::Compile(local_ctx, src, &script_origin).ToLocal(&script)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  Local<Value> result;
  if (!script->Run(local_ctx).ToLocal(&result)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, result);

  rtn.value = tracked_value(ctx, val);
  return rtn;
}

/********** UnboundScript & ScriptCompilerCachedData **********/

ScriptCompilerCachedData* UnboundScriptCreateCodeCache(
    IsolatePtr iso,
    UnboundScriptPtr us_ptr) {
  ISOLATE_SCOPE(iso);

  Local<UnboundScript> unbound_script = us_ptr->ptr.Get(iso);

  ScriptCompiler::CachedData* cached_data =
      ScriptCompiler::CreateCodeCache(unbound_script);

  ScriptCompilerCachedData* cd = new ScriptCompilerCachedData;
  cd->ptr = cached_data;
  cd->data = cached_data->data;
  cd->length = cached_data->length;
  cd->rejected = cached_data->rejected;
  return cd;
}

void ScriptCompilerCachedDataDelete(ScriptCompilerCachedData* cached_data) {
  delete cached_data->ptr;
  delete cached_data;
}

// This can only run in contexts that belong to the same isolate
// the script was compiled in
RtnValue UnboundScriptRun(ContextPtr ctx, UnboundScriptPtr us_ptr) {
  LOCAL_CONTEXT(ctx)

  RtnValue rtn = {};

  Local<UnboundScript> unbound_script = us_ptr->ptr.Get(iso);

  Local<Script> script = unbound_script->BindToCurrentContext();
  Local<Value> result;
  if (!script->Run(local_ctx).ToLocal(&result)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, result);

  rtn.value = tracked_value(ctx, val);
  return rtn;
}

RtnValue JSONParse(ContextPtr ctx, const char* str) {
  LOCAL_CONTEXT(ctx);
  RtnValue rtn = {};

  Local<String> v8Str;
  if (!String::NewFromUtf8(iso, str, NewStringType::kNormal).ToLocal(&v8Str)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
  }

  Local<Value> result;
  if (!JSON::Parse(local_ctx, v8Str).ToLocal(&result)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, result);

  rtn.value = tracked_value(ctx, val);
  return rtn;
}

const char* JSONStringify(ContextPtr ctx, ValuePtr val) {
  Isolate* iso;
  Local<Context> local_ctx;

  if (ctx != nullptr) {
    iso = ctx->iso;
  } else {
    iso = val->iso;
  }

  Locker locker(iso);
  Isolate::Scope isolate_scope(iso);
  HandleScope handle_scope(iso);

  if (ctx != nullptr) {
    local_ctx = ctx->ptr.Get(iso);
  } else {
    if (val->ctx != nullptr) {
      local_ctx = val->ctx->ptr.Get(iso);
    } else {
      m_ctx* ctx = isolateInternalContext(iso);
      local_ctx = ctx->ptr.Get(iso);
    }
  }

  Context::Scope context_scope(local_ctx);

  Local<String> str;
  if (!JSON::Stringify(local_ctx, val->ptr.Get(iso)).ToLocal(&str)) {
    return nullptr;
  }
  String::Utf8Value json(iso, str);
  return CopyString(json);
}

void ValueRelease(ValuePtr ptr) {
  if (ptr == nullptr) {
    return;
  }

  ptr->ctx->vals.erase(ptr->id);
  ptr->ptr.Reset();
  delete ptr;
}

// Two contexts may reach into each other's objects exactly when they carry the
// same security token. Without one, V8's default access check fires on any
// cross-context property access through a global proxy and the read throws "no
// access" — which is what a browser wants for a cross-origin frame, and exactly
// what it must not do for a same-origin one.
//
// The token is any value; identity is what is compared. An embedder that wants
// two contexts same-origin reads one's token and sets it on the other.
void ContextSetSecurityToken(ContextPtr ctx, ValuePtr token_ptr) {
  LOCAL_CONTEXT(ctx);
  if (token_ptr == nullptr) {
    local_ctx->UseDefaultSecurityToken();
    return;
  }
  local_ctx->SetSecurityToken(token_ptr->ptr.Get(iso));
}

void ContextSetImportMetaInitializer(ContextPtr ctx, ValuePtr fn_ptr) {
  LOCAL_CONTEXT(ctx);
  if (fn_ptr == nullptr) {
    ctx->importMetaInit.Reset();
    return;
  }
  Local<Value> fn = fn_ptr->ptr.Get(iso);
  if (!fn->IsFunction()) {
    ctx->importMetaInit.Reset();
    return;
  }
  ctx->importMetaInit.Reset(iso, fn.As<Function>());
}

void ContextAllowCodeGeneration(ContextPtr ctx,
                                int allow,
                                const char* message) {
  LOCAL_CONTEXT(ctx);
  local_ctx->AllowCodeGenerationFromStrings(allow != 0);
  if (allow == 0 && message != nullptr) {
    local_ctx->SetErrorMessageForCodeGenerationFromStrings(
        String::NewFromUtf8(iso, message).ToLocalChecked());
  }
}

ValuePtr ContextSecurityToken(ContextPtr ctx) {
  LOCAL_CONTEXT(ctx);
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, local_ctx->GetSecurityToken());
  return tracked_value(ctx, val);
}

ValuePtr ContextTakeException(ContextPtr ctx) {
  LOCAL_CONTEXT(ctx);
  m_ctx* ictx = isolateInternalContext(iso);
  if (ictx == nullptr || ictx->lastException.IsEmpty()) {
    return nullptr;
  }
  Local<Value> exception = ictx->lastException.Get(iso);
  ictx->lastException.Reset();
  // An error the embedder's own compile raised belongs to the internal
  // context; handing it to a page would hand the page that realm.
  if (exception->IsObject() && ictx != ctx) {
    Local<Context> creation;
    if (exception.As<Object>()->GetCreationContext(iso).ToLocal(&creation) &&
        creation == ictx->ptr.Get(iso)) {
      return nullptr;
    }
  }
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, exception);
  return tracked_value(ctx, val);
}

ValuePtr ContextGlobal(ContextPtr ctx) {
  LOCAL_CONTEXT(ctx);
  m_value* val = new m_value;
  val->id = 0;

  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(
      iso, local_ctx->Global());

  return tracked_value(ctx, val);
}

/********** Value **********/

#define LOCAL_VALUE(val)                   \
  Isolate* iso = val->iso;                 \
  Locker locker(iso);                      \
  Isolate::Scope isolate_scope(iso);       \
  HandleScope handle_scope(iso);           \
  TryCatch try_catch(iso);                 \
  m_ctx* ctx = val->ctx;                   \
  Local<Context> local_ctx;                \
  if (ctx != nullptr) {                    \
    local_ctx = ctx->ptr.Get(iso);         \
  } else {                                 \
    ctx = isolateInternalContext(iso);     \
    local_ctx = ctx->ptr.Get(iso);         \
  }                                        \
  ContextUse ctx_use(ctx);                 \
  Context::Scope context_scope(local_ctx); \
  Local<Value> value = val->ptr.Get(iso);

ValuePtr NewValueInteger(IsolatePtr iso, int32_t v) {
  ISOLATE_SCOPE_INTERNAL_CONTEXT(iso);
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(
      iso, Integer::New(iso, v));
  return tracked_value(ctx, val);
}

ValuePtr NewValueIntegerFromUnsigned(IsolatePtr iso, uint32_t v) {
  ISOLATE_SCOPE_INTERNAL_CONTEXT(iso);
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(
      iso, Integer::NewFromUnsigned(iso, v));
  return tracked_value(ctx, val);
}

RtnValue NewValueString(IsolatePtr iso, const char* v, int v_length) {
  ISOLATE_SCOPE_INTERNAL_CONTEXT(iso);
  TryCatch try_catch(iso);
  RtnValue rtn = {};
  Local<String> str;
  if (!String::NewFromUtf8(iso, v, NewStringType::kNormal, v_length)
           .ToLocal(&str)) {
    rtn.error = ExceptionError(try_catch, iso, ctx->ptr.Get(iso));
    return rtn;
  }
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, str);
  rtn.value = tracked_value(ctx, val);
  return rtn;
}

// GoExternalOneByteResource is the V8 String backing for memory owned by
// the Go runtime. The class holds only a pointer + length into the Go
// []byte and a pin id — it does NOT copy the data. The Go side keeps the
// underlying slice alive (pinned in a process-global map keyed by pin_id)
// until V8 disposes the resource. V8 calls Dispose() when the wrapping
// String is collected, which deletes this resource via the default
// implementation; ~GoExternalOneByteResource then notifies Go through the
// cgo-exported goReleaseExternalString to drop the pin.
//
// Constraints:
//   - data must point to ASCII / Latin-1 (one byte per code point); V8
//     reads it as raw one-byte content, no UTF-8 decoding.
//   - data must not be modified or relocated for the lifetime of the
//     resource. Go heap allocations are stable (Go GC does not compact),
//     so as long as the pin keeps a Go reference alive this holds.
class GoExternalOneByteResource
    : public String::ExternalOneByteStringResource {
 public:
  GoExternalOneByteResource(const char* data, size_t length, uint64_t pin_id)
      : data_(data), length_(length), pin_id_(pin_id) {}
  ~GoExternalOneByteResource() override {
    goReleaseExternalString(pin_id_);
  }
  const char* data() const override { return data_; }
  size_t length() const override { return length_; }

 private:
  const char* data_;
  size_t length_;
  uint64_t pin_id_;
};

RtnValue NewExternalOneByteString(IsolatePtr iso,
                                   const char* v,
                                   int v_length,
                                   uint64_t pin_id) {
  ISOLATE_SCOPE_INTERNAL_CONTEXT(iso);
  TryCatch try_catch(iso);
  RtnValue rtn = {};
  // V8 takes ownership of the resource and deletes it via Dispose() when
  // the wrapping String becomes unreachable. On the failure path V8 has
  // not taken ownership, so we delete it ourselves.
  GoExternalOneByteResource* res =
      new GoExternalOneByteResource(v, static_cast<size_t>(v_length), pin_id);
  Local<String> str;
  if (!String::NewExternalOneByte(iso, res).ToLocal(&str)) {
    delete res;
    rtn.error = ExceptionError(try_catch, iso, ctx->ptr.Get(iso));
    return rtn;
  }
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, str);
  rtn.value = tracked_value(ctx, val);
  return rtn;
}

ValuePtr NewValueNull(IsolatePtr iso) {
  ISOLATE_SCOPE_INTERNAL_CONTEXT(iso);
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, Null(iso));
  return tracked_value(ctx, val);
}

ValuePtr NewValueUndefined(IsolatePtr iso) {
  ISOLATE_SCOPE_INTERNAL_CONTEXT(iso);
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr =
      Global<Value>(iso, Undefined(iso));
  return tracked_value(ctx, val);
}

ValuePtr NewValueBoolean(IsolatePtr iso, int v) {
  ISOLATE_SCOPE_INTERNAL_CONTEXT(iso);
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(
      iso, Boolean::New(iso, v));
  return tracked_value(ctx, val);
}

ValuePtr NewValueNumber(IsolatePtr iso, double v) {
  ISOLATE_SCOPE_INTERNAL_CONTEXT(iso);
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(
      iso, Number::New(iso, v));
  return tracked_value(ctx, val);
}

ValuePtr NewValueBigInt(IsolatePtr iso, int64_t v) {
  ISOLATE_SCOPE_INTERNAL_CONTEXT(iso);
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(
      iso, BigInt::New(iso, v));
  return tracked_value(ctx, val);
}

ValuePtr NewValueBigIntFromUnsigned(IsolatePtr iso, uint64_t v) {
  ISOLATE_SCOPE_INTERNAL_CONTEXT(iso);
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(
      iso, BigInt::NewFromUnsigned(iso, v));
  return tracked_value(ctx, val);
}

RtnValue NewValueBigIntFromWords(IsolatePtr iso,
                                 int sign_bit,
                                 int word_count,
                                 const uint64_t* words) {
  ISOLATE_SCOPE_INTERNAL_CONTEXT(iso);
  TryCatch try_catch(iso);
  Local<Context> local_ctx = ctx->ptr.Get(iso);

  RtnValue rtn = {};
  Local<BigInt> bigint;
  if (!BigInt::NewFromWords(local_ctx, sign_bit, word_count, words)
           .ToLocal(&bigint)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, bigint);
  rtn.value = tracked_value(ctx, val);
  return rtn;
}

const uint32_t* ValueToArrayIndex(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  Local<Uint32> array_index;
  if (!value->ToArrayIndex(local_ctx).ToLocal(&array_index)) {
    return nullptr;
  }

  uint32_t* idx = (uint32_t*)malloc(sizeof(uint32_t));
  *idx = array_index->Value();
  return idx;
}

int ValueToBoolean(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->BooleanValue(iso);
}

int32_t ValueToInt32(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->Int32Value(local_ctx).FromMaybe(0);
}

int64_t ValueToInteger(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IntegerValue(local_ctx).FromMaybe(0);
}

double ValueToNumber(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->NumberValue(local_ctx).FromMaybe(
      std::numeric_limits<double>::quiet_NaN());
}

RtnString ValueToDetailString(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  RtnString rtn = {0};
  Local<String> str;
  if (!value->ToDetailString(local_ctx).ToLocal(&str)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  String::Utf8Value ds(iso, str);
  rtn.data = CopyString(ds);
  rtn.length = ds.length();
  return rtn;
}

RtnString ValueToString(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  RtnString rtn = {0};
  // String::Utf8Value will result in an empty string if conversion to a string
  // fails
  // TODO: Consider propagating the JS error. A fallback value could be returned
  // in Value.String()
  String::Utf8Value src(iso, value);
  char* data = static_cast<char*>(malloc(src.length()));
  memcpy(data, *src, src.length());
  rtn.data = data;
  rtn.length = src.length();
  return rtn;
}

RtnString ValueToWTF8String(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  RtnString rtn = {0};
  Local<String> str;
  if (!value->ToString(local_ctx).ToLocal(&str)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  // Without kReplaceInvalidUtf8 an unpaired surrogate is written as its own
  // three-byte sequence (WTF-8), so the string round-trips exactly.
  size_t length = str->Utf8LengthV2(iso);
  char* data = static_cast<char*>(malloc(length + 1));
  size_t written = str->WriteUtf8V2(iso, data, length + 1,
                                    String::WriteFlags::kNullTerminate);
  rtn.data = data;
  rtn.length = written > 0 ? written - 1 : 0;
  return rtn;
}

RtnValue NewValueStringUTF16(IsolatePtr iso,
                             const uint16_t* v,
                             int v_length) {
  ISOLATE_SCOPE_INTERNAL_CONTEXT(iso);
  TryCatch try_catch(iso);
  RtnValue rtn = {};
  Local<String> str;
  if (!String::NewFromTwoByte(iso, v, NewStringType::kNormal, v_length)
           .ToLocal(&str)) {
    rtn.error = ExceptionError(try_catch, iso, ctx->ptr.Get(iso));
    return rtn;
  }
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, str);
  rtn.value = tracked_value(ctx, val);
  return rtn;
}

uint32_t ValueToUint32(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->Uint32Value(local_ctx).FromMaybe(0);
}

ValueBigInt ValueToBigInt(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  Local<BigInt> bint;
  if (!value->ToBigInt(local_ctx).ToLocal(&bint)) {
    return {nullptr, 0};
  }

  int word_count = bint->WordCount();
  int sign_bit = 0;
  uint64_t* words = (uint64_t*)malloc(sizeof(uint64_t) * word_count);
  bint->ToWordsArray(&sign_bit, &word_count, words);
  ValueBigInt rtn = {words, word_count, sign_bit};
  return rtn;
}

RtnValue ValueToObject(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  RtnValue rtn = {};
  Local<Object> obj;
  if (!value->ToObject(local_ctx).ToLocal(&obj)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  m_value* new_val = new m_value;
  new_val->id = 0;
  new_val->iso = iso;
  new_val->ctx = ctx;
  new_val->ptr = Global<Value>(iso, obj);
  rtn.value = tracked_value(ctx, new_val);
  return rtn;
}

int ValueSameValue(ValuePtr val1, ValuePtr val2) {
  Isolate* iso = val1->iso;
  ISOLATE_SCOPE(iso);
  Local<Value> value1 = val1->ptr.Get(iso);
  Local<Value> value2 = val2->ptr.Get(iso);

  return value1->SameValue(value2);
}

int ValueIdentityHash(ValuePtr val) {
  Isolate* iso = val->iso;
  ISOLATE_SCOPE(iso);
  Local<Value> value = val->ptr.Get(iso);
  if (!value->IsObject()) return 0;
  return Local<Object>::Cast(value)->GetIdentityHash();
}

int ValueIsUndefined(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsUndefined();
}

int ValueIsNull(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsNull();
}

int ValueIsNullOrUndefined(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsNullOrUndefined();
}

int ValueIsTrue(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsTrue();
}

int ValueIsFalse(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsFalse();
}

int ValueIsName(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsName();
}

int ValueIsString(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsString();
}

int ValueIsSymbol(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsSymbol();
}

int ValueIsFunction(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsFunction();
}

int ValueIsObject(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsObject();
}

int ValueIsBigInt(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsBigInt();
}

int ValueIsBoolean(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsBoolean();
}

int ValueIsNumber(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsNumber();
}

int ValueIsExternal(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsExternal();
}

int ValueIsInt32(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsInt32();
}

int ValueIsUint32(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsUint32();
}

int ValueIsDate(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsDate();
}

int ValueIsArgumentsObject(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsArgumentsObject();
}

int ValueIsBigIntObject(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsBigIntObject();
}

int ValueIsNumberObject(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsNumberObject();
}

int ValueIsStringObject(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsStringObject();
}

int ValueIsSymbolObject(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsSymbolObject();
}

int ValueIsNativeError(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsNativeError();
}

int ValueIsRegExp(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsRegExp();
}

int ValueIsAsyncFunction(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsAsyncFunction();
}

int ValueIsGeneratorFunction(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsGeneratorFunction();
}

int ValueIsGeneratorObject(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsGeneratorObject();
}

int ValueIsPromise(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsPromise();
}

int ValueIsMap(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsMap();
}

int ValueIsSet(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsSet();
}

int ValueIsMapIterator(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsMapIterator();
}

int ValueIsSetIterator(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsSetIterator();
}

int ValueIsWeakMap(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsWeakMap();
}

int ValueIsWeakSet(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsWeakSet();
}

int ValueIsArray(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsArray();
}

int ValueIsArrayBuffer(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsArrayBuffer();
}

int ValueIsArrayBufferView(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsArrayBufferView();
}

int ValueIsTypedArray(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsTypedArray();
}

int ValueIsUint8Array(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsUint8Array();
}

int ValueIsUint8ClampedArray(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsUint8ClampedArray();
}

int ValueIsInt8Array(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsInt8Array();
}

int ValueIsUint16Array(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsUint16Array();
}

int ValueIsInt16Array(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsInt16Array();
}

int ValueIsUint32Array(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsUint32Array();
}

int ValueIsInt32Array(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsInt32Array();
}

int ValueIsFloat32Array(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsFloat32Array();
}

int ValueIsFloat64Array(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsFloat64Array();
}

int ValueIsBigInt64Array(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsBigInt64Array();
}

int ValueIsBigUint64Array(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsBigUint64Array();
}

int ValueIsDataView(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsDataView();
}

int ValueIsSharedArrayBuffer(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsSharedArrayBuffer();
}

int ValueIsProxy(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsProxy();
}

int ValueIsWasmModuleObject(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsWasmModuleObject();
}

int ValueIsModuleNamespaceObject(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IsModuleNamespaceObject();
}

/********** Object **********/

#define LOCAL_OBJECT(ptr) \
  LOCAL_VALUE(ptr)        \
  Local<Object> obj = value.As<Object>()

void ObjectSet(ValuePtr ptr, const char* key, ValuePtr prop_val) {
  LOCAL_OBJECT(ptr);
  Local<String> key_val =
      String::NewFromUtf8(iso, key, NewStringType::kNormal).ToLocalChecked();
  obj->Set(local_ctx, key_val, prop_val->ptr.Get(iso)).Check();
}

void ObjectSetIdx(ValuePtr ptr, uint32_t idx, ValuePtr prop_val) {
  LOCAL_OBJECT(ptr);
  obj->Set(local_ctx, idx, prop_val->ptr.Get(iso)).Check();
}

int ObjectSetInternalField(ValuePtr ptr, int idx, ValuePtr val_ptr) {
  LOCAL_OBJECT(ptr);
  m_value* prop_val = static_cast<m_value*>(val_ptr);

  if (idx >= obj->InternalFieldCount()) {
    return 0;
  }

  obj->SetInternalField(idx, prop_val->ptr.Get(iso));

  return 1;
}

int ObjectInternalFieldCount(ValuePtr ptr) {
  LOCAL_OBJECT(ptr);
  return obj->InternalFieldCount();
}

// A property keyed by a Value rather than a string: a Symbol, in practice.
// Symbol.for(...) keys are what the engine hangs its per-object slots on --
// getOwnPropertyNames does not list them and a page cannot guess them.
void ObjectSetValueKey(ValuePtr ptr, ValuePtr key_ptr, ValuePtr prop_val) {
  LOCAL_OBJECT(ptr);
  Local<Value> key_val = key_ptr->ptr.Get(iso);
  obj->Set(local_ctx, key_val, prop_val->ptr.Get(iso)).Check();
}

RtnValue ObjectGetValueKey(ValuePtr ptr, ValuePtr key_ptr) {
  LOCAL_OBJECT(ptr);
  RtnValue rtn = {};
  Local<Value> key_val = key_ptr->ptr.Get(iso);
  Local<Value> result;
  if (!obj->Get(local_ctx, key_val).ToLocal(&result)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  m_value* new_val = new m_value;
  new_val->id = 0;
  new_val->iso = iso;
  new_val->ctx = ctx;
  new_val->ptr = Global<Value>(iso, result);
  rtn.value = tracked_value(ctx, new_val);
  return rtn;
}

// A property keyed by a v8::Private: invisible to script entirely -- not
// listed by Reflect.ownKeys or getOwnPropertySymbols, not reachable by any
// key a script can build, not seen by a Proxy trap. Private::ForApi answers
// the same private for the same name everywhere in the isolate, so an
// embedder names its slot by a string and never holds a handle to it.
void ObjectSetPrivate(ValuePtr ptr, const char* name, ValuePtr prop_val) {
  LOCAL_OBJECT(ptr);
  Local<String> name_val =
      String::NewFromUtf8(iso, name, NewStringType::kNormal).ToLocalChecked();
  Local<Private> key = Private::ForApi(iso, name_val);
  obj->SetPrivate(local_ctx, key, prop_val->ptr.Get(iso)).Check();
}

RtnValue ObjectGetPrivate(ValuePtr ptr, const char* name) {
  LOCAL_OBJECT(ptr);
  RtnValue rtn = {};
  Local<String> name_val =
      String::NewFromUtf8(iso, name, NewStringType::kNormal).ToLocalChecked();
  Local<Private> key = Private::ForApi(iso, name_val);
  Local<Value> result;
  if (!obj->GetPrivate(local_ctx, key).ToLocal(&result)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  m_value* new_val = new m_value;
  new_val->id = 0;
  new_val->iso = iso;
  new_val->ctx = ctx;
  new_val->ptr = Global<Value>(iso, result);
  rtn.value = tracked_value(ctx, new_val);
  return rtn;
}

void ObjectDeletePrivate(ValuePtr ptr, const char* name) {
  LOCAL_OBJECT(ptr);
  Local<String> name_val =
      String::NewFromUtf8(iso, name, NewStringType::kNormal).ToLocalChecked();
  Local<Private> key = Private::ForApi(iso, name_val);
  obj->DeletePrivate(local_ctx, key).Check();
}

// A private symbol as a VALUE, for script the embedder writes: `obj[key]`
// with it reads and writes a property no page script can list, name or
// reach -- getOwnPropertySymbols, Reflect.ownKeys and a Proxy's ownKeys all
// skip it, and Symbol.for never answers it. It is the same private
// ObjectSetPrivate keys by name (Private::ForApi), so the embedder's Go side
// and its scripts agree on one slot. V8 reads a private as a Name throughout
// (Object::GetPrivate is Get with the key cast to a Value); what it does not
// do is walk the prototype chain for one, so a private-keyed property is
// found on the object that owns it and nowhere else.
//
// The embedder's script must not hand the value to page script: anyone
// holding it can use it.
static Local<Value> PrivateAsValue(Isolate* iso, Local<String> name) {
  Local<Private> key = Private::ForApi(iso, name);
  return key.As<Value>();
}

ValuePtr ContextPrivateSymbol(ContextPtr ctx, const char* name) {
  LOCAL_CONTEXT(ctx);
  Local<String> name_val =
      String::NewFromUtf8(iso, name, NewStringType::kNormal).ToLocalChecked();
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, PrivateAsValue(iso, name_val));
  return tracked_value(ctx, val);
}

// mint(name) is ContextPrivateSymbol(name). mint(description, true) is a
// FRESH private symbol, Private::New: the private counterpart of
// Symbol(description), unique per call and collected like any other value.
static void MintPrivateSymbol(const FunctionCallbackInfo<Value>& info) {
  if (info.Length() < 1 || !info[0]->IsString()) {
    return;
  }
  Isolate* iso = info.GetIsolate();
  Local<String> name = info[0].As<String>();
  if (info.Length() > 1 && info[1]->IsTrue()) {
    info.GetReturnValue().Set(Private::New(iso, name).As<Value>());
    return;
  }
  info.GetReturnValue().Set(PrivateAsValue(iso, name));
}

// A function of this context that answers the private symbol for a string,
// as ContextPrivateSymbol does -- or, with a second argument true, a fresh
// one -- all in C++, no crossing into Go. The embedder hands it to its own
// scripts only.
RtnValue ContextPrivateSymbolFunction(ContextPtr ctx) {
  LOCAL_CONTEXT(ctx);
  RtnValue rtn = {};
  Local<Function> fn;
  if (!Function::New(local_ctx, MintPrivateSymbol, Local<Value>(), 1,
                     ConstructorBehavior::kThrow)
           .ToLocal(&fn)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, fn);
  rtn.value = tracked_value(ctx, val);
  return rtn;
}

RtnValue ObjectGet(ValuePtr ptr, const char* key) {
  LOCAL_OBJECT(ptr);
  RtnValue rtn = {};

  Local<String> key_val;
  if (!String::NewFromUtf8(iso, key, NewStringType::kNormal)
           .ToLocal(&key_val)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  Local<Value> result;
  if (!obj->Get(local_ctx, key_val).ToLocal(&result)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  m_value* new_val = new m_value;
  new_val->id = 0;
  new_val->iso = iso;
  new_val->ctx = ctx;
  new_val->ptr =
      Global<Value>(iso, result);

  rtn.value = tracked_value(ctx, new_val);
  return rtn;
}

ValuePtr ObjectGetInternalField(ValuePtr ptr, int idx) {
  LOCAL_OBJECT(ptr);

  if (idx >= obj->InternalFieldCount()) {
    return nullptr;
  }

  // V8 14.x: GetInternalField returns Local<Data> (base class of Value)
  // since internal fields can hold arbitrary embedder data. v8go only sets
  // Values via ObjectSetInternalField, so the downcast is safe here.
  Local<Value> result = obj->GetInternalField(idx).As<Value>();

  m_value* new_val = new m_value;
  new_val->id = 0;
  new_val->iso = iso;
  new_val->ctx = ctx;
  new_val->ptr =
      Global<Value>(iso, result);

  return tracked_value(ctx, new_val);
}

RtnValue ObjectGetIdx(ValuePtr ptr, uint32_t idx) {
  LOCAL_OBJECT(ptr);
  RtnValue rtn = {};

  Local<Value> result;
  if (!obj->Get(local_ctx, idx).ToLocal(&result)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  m_value* new_val = new m_value;
  new_val->id = 0;
  new_val->iso = iso;
  new_val->ctx = ctx;
  new_val->ptr =
      Global<Value>(iso, result);

  rtn.value = tracked_value(ctx, new_val);
  return rtn;
}

int ObjectHas(ValuePtr ptr, const char* key) {
  LOCAL_OBJECT(ptr);
  Local<String> key_val =
      String::NewFromUtf8(iso, key, NewStringType::kNormal).ToLocalChecked();
  return obj->Has(local_ctx, key_val).FromMaybe(false);
}

int ObjectHasIdx(ValuePtr ptr, uint32_t idx) {
  LOCAL_OBJECT(ptr);
  return obj->Has(local_ctx, idx).FromMaybe(false);
}

int ObjectDelete(ValuePtr ptr, const char* key) {
  LOCAL_OBJECT(ptr);
  Local<String> key_val =
      String::NewFromUtf8(iso, key, NewStringType::kNormal).ToLocalChecked();
  return obj->Delete(local_ctx, key_val).FromMaybe(false);
}

int ObjectDeleteIdx(ValuePtr ptr, uint32_t idx) {
  LOCAL_OBJECT(ptr);
  return obj->Delete(local_ctx, idx).FromMaybe(false);
}

/********** Promise **********/

RtnValue NewPromiseResolver(ContextPtr ctx) {
  LOCAL_CONTEXT(ctx);
  RtnValue rtn = {};
  Local<Promise::Resolver> resolver;
  if (!Promise::Resolver::New(local_ctx).ToLocal(&resolver)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, resolver);
  rtn.value = tracked_value(ctx, val);
  return rtn;
}

ValuePtr PromiseResolverGetPromise(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  Local<Promise::Resolver> resolver = value.As<Promise::Resolver>();
  Local<Promise> promise = resolver->GetPromise();
  m_value* promise_val = new m_value;
  promise_val->id = 0;
  promise_val->iso = iso;
  promise_val->ctx = ctx;
  promise_val->ptr =
      Global<Value>(iso, promise);
  return tracked_value(ctx, promise_val);
}

int PromiseResolverResolve(ValuePtr ptr, ValuePtr resolve_val) {
  LOCAL_VALUE(ptr);
  Local<Promise::Resolver> resolver = value.As<Promise::Resolver>();
  return resolver->Resolve(local_ctx, resolve_val->ptr.Get(iso)).FromMaybe(false);
}

int PromiseResolverReject(ValuePtr ptr, ValuePtr reject_val) {
  LOCAL_VALUE(ptr);
  Local<Promise::Resolver> resolver = value.As<Promise::Resolver>();
  return resolver->Reject(local_ctx, reject_val->ptr.Get(iso)).FromMaybe(false);
}

int PromiseState(ValuePtr ptr) {
  LOCAL_VALUE(ptr)
  Local<Promise> promise = value.As<Promise>();
  return promise->State();
}

RtnValue PromiseThen(ValuePtr ptr, int callback_ref) {
  LOCAL_VALUE(ptr)
  RtnValue rtn = {};
  Local<Promise> promise = value.As<Promise>();
  Local<Integer> cbData = Integer::New(iso, callback_ref);
  Local<Function> func;
  if (!Function::New(local_ctx, FunctionTemplateCallback, cbData)
           .ToLocal(&func)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  Local<Promise> result;
  if (!promise->Then(local_ctx, func).ToLocal(&result)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  m_value* result_val = new m_value;
  result_val->id = 0;
  result_val->iso = iso;
  result_val->ctx = ctx;
  result_val->ptr =
      Global<Value>(iso, result);
  rtn.value = tracked_value(ctx, result_val);
  return rtn;
}

RtnValue PromiseThen2(ValuePtr ptr, int on_fulfilled_ref, int on_rejected_ref) {
  LOCAL_VALUE(ptr)
  RtnValue rtn = {};
  Local<Promise> promise = value.As<Promise>();
  Local<Integer> onFulfilledData = Integer::New(iso, on_fulfilled_ref);
  Local<Function> onFulfilledFunc;
  if (!Function::New(local_ctx, FunctionTemplateCallback, onFulfilledData)
           .ToLocal(&onFulfilledFunc)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  Local<Integer> onRejectedData = Integer::New(iso, on_rejected_ref);
  Local<Function> onRejectedFunc;
  if (!Function::New(local_ctx, FunctionTemplateCallback, onRejectedData)
           .ToLocal(&onRejectedFunc)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  Local<Promise> result;
  if (!promise->Then(local_ctx, onFulfilledFunc, onRejectedFunc)
           .ToLocal(&result)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  m_value* result_val = new m_value;
  result_val->id = 0;
  result_val->iso = iso;
  result_val->ctx = ctx;
  result_val->ptr =
      Global<Value>(iso, result);
  rtn.value = tracked_value(ctx, result_val);
  return rtn;
}

RtnValue PromiseCatch(ValuePtr ptr, int callback_ref) {
  LOCAL_VALUE(ptr)
  RtnValue rtn = {};
  Local<Promise> promise = value.As<Promise>();
  Local<Integer> cbData = Integer::New(iso, callback_ref);
  Local<Function> func;
  if (!Function::New(local_ctx, FunctionTemplateCallback, cbData)
           .ToLocal(&func)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  Local<Promise> result;
  if (!promise->Catch(local_ctx, func).ToLocal(&result)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  m_value* result_val = new m_value;
  result_val->id = 0;
  result_val->iso = iso;
  result_val->ctx = ctx;
  result_val->ptr =
      Global<Value>(iso, result);
  rtn.value = tracked_value(ctx, result_val);
  return rtn;
}

ValuePtr PromiseResult(ValuePtr ptr) {
  LOCAL_VALUE(ptr)
  Local<Promise> promise = value.As<Promise>();
  Local<Value> result = promise->Result();
  m_value* result_val = new m_value;
  result_val->id = 0;
  result_val->iso = iso;
  result_val->ctx = ctx;
  result_val->ptr =
      Global<Value>(iso, result);
  return tracked_value(ctx, result_val);
}

/********** Function **********/

static void buildCallArguments(Isolate* iso,
                               Local<Value>* argv,
                               int argc,
                               ValuePtr args[]) {
  for (int i = 0; i < argc; i++) {
    argv[i] = args[i]->ptr.Get(iso);
  }
}

RtnValue FunctionCall(ValuePtr ptr, ValuePtr recv, int argc, ValuePtr args[]) {
  LOCAL_VALUE(ptr)

  RtnValue rtn = {};
  Local<Function> fn = Local<Function>::Cast(value);
  Local<Value> argv[argc];
  buildCallArguments(iso, argv, argc, args);

  Local<Value> local_recv = recv->ptr.Get(iso);

  Local<Value> result;
  if (!fn->Call(local_ctx, local_recv, argc, argv).ToLocal(&result)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  m_value* rtnval = new m_value;
  rtnval->id = 0;
  rtnval->iso = iso;
  rtnval->ctx = ctx;
  rtnval->ptr = Global<Value>(iso, result);
  rtn.value = tracked_value(ctx, rtnval);
  return rtn;
}

RtnValue FunctionNewInstance(ValuePtr ptr, int argc, ValuePtr args[]) {
  LOCAL_VALUE(ptr)
  RtnValue rtn = {};
  Local<Function> fn = Local<Function>::Cast(value);
  Local<Value> argv[argc];
  buildCallArguments(iso, argv, argc, args);
  Local<Object> result;
  if (!fn->NewInstance(local_ctx, argc, argv).ToLocal(&result)) {
    rtn.error = ExceptionError(try_catch, iso, local_ctx);
    return rtn;
  }
  m_value* rtnval = new m_value;
  rtnval->id = 0;
  rtnval->iso = iso;
  rtnval->ctx = ctx;
  rtnval->ptr = Global<Value>(iso, result);
  rtn.value = tracked_value(ctx, rtnval);
  return rtn;
}

ValuePtr FunctionSourceMapUrl(ValuePtr ptr) {
  LOCAL_VALUE(ptr)
  Local<Function> fn = Local<Function>::Cast(value);
  Local<Value> result = fn->GetScriptOrigin().SourceMapUrl().As<Value>();
  m_value* rtnval = new m_value;
  rtnval->id = 0;
  rtnval->iso = iso;
  rtnval->ctx = ctx;
  rtnval->ptr = Global<Value>(iso, result);
  return tracked_value(ctx, rtnval);
}

void FunctionSetName(ValuePtr ptr, const char* name, int len) {
  LOCAL_VALUE(ptr)
  Local<Function> fn = Local<Function>::Cast(value);
  Local<String> str =
      String::NewFromUtf8(iso, name, NewStringType::kNormal, len)
          .ToLocalChecked();
  fn->SetName(str);
}

/********** v8::V8 **********/

const char* Version() {
  return V8::GetVersion();
}

void SetFlags(const char* flags) {
  V8::SetFlagsFromString(flags);
}

// The ref of the context the embedder last entered, or of the context a
// running microtask belongs to -- whose code is running, as the embedder
// sees it, including after an `await`, where nothing the embedder entered is
// on the stack any more. 0 with no context entered, or for a context v8go
// did not make.
int IsolateEnteredOrMicrotaskContextRef(IsolatePtr iso) {
  ISOLATE_SCOPE(iso);
  if (!iso->InContext()) {
    return 0;
  }
  Local<Context> context = iso->GetEnteredOrMicrotaskContext();
  if (context.IsEmpty()) {
    return 0;
  }
  Local<Value> ref_val = context->GetEmbedderData(1);
  if (ref_val.IsEmpty() || !ref_val->IsInt32()) {
    return 0;
  }
  return ref_val.As<Integer>()->Value();
}

// Runs one foreground task the platform holds for this isolate, if any:
// what V8's own background work — asynchronous WebAssembly compilation,
// streaming, GC finalisation callbacks — posts back to the isolate's thread.
// Nothing in V8 runs these on its own; an embedder that never pumps sees
// WebAssembly.instantiate's promise stay pending forever. Returns 1 when a
// task ran, so the caller can loop until the queue is empty.
int IsolatePumpMessageLoop(IsolatePtr iso) {
  ISOLATE_SCOPE(iso);
  return platform::PumpMessageLoop(default_platform.get(), iso,
                                   platform::MessageLoopBehavior::kDoNotWait)
             ? 1
             : 0;
}

/********** SharedArrayBuffer & BackingStore ***********/

struct v8BackingStore {
  v8BackingStore(std::shared_ptr<v8::BackingStore>&& ptr)
      : backing_store{ptr} {}
  std::shared_ptr<v8::BackingStore> backing_store;
};

BackingStorePtr SharedArrayBufferGetBackingStore(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  auto buffer = Local<SharedArrayBuffer>::Cast(value);
  auto backing_store = buffer->GetBackingStore();
  auto proxy = new v8BackingStore(std::move(backing_store));
  return proxy;
}

BackingStorePtr ArrayBufferGetBackingStore(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  auto buffer = Local<ArrayBuffer>::Cast(value);
  auto backing_store = buffer->GetBackingStore();
  auto proxy = new v8BackingStore(std::move(backing_store));
  return proxy;
}

BackingStorePtr TypedArrayGetBuffer(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  auto view = Local<TypedArray>::Cast(value);
  auto backing_store = view->Buffer()->GetBackingStore();
  auto proxy = new v8BackingStore(std::move(backing_store));
  return proxy;
}

size_t TypedArrayByteOffset(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  auto view = Local<TypedArray>::Cast(value);
  return view->ByteOffset();
}

size_t TypedArrayByteLength(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  auto view = Local<TypedArray>::Cast(value);
  return view->ByteLength();
}

// V8 14.7 dropped the ArrayBuffer::New(data, len, kInternalized) overload, so
// we allocate via ArrayBuffer::New(iso, byte_length) (V8-owned memory) and copy
// the bytes into the freshly created buffer's Data(). The caller's buffer may
// be freed as soon as this returns. A Context::Scope is required because
// ArrayBuffer::New builds a JS object (it looks up the native context for the
// initial map / prototype), unlike Integer/String::New.
ValuePtr NewArrayBuffer(IsolatePtr iso, void* data, int length) {
  ISOLATE_SCOPE(iso);
  m_ctx* ctx = isolateInternalContext(iso);
  Context::Scope context_scope(ctx->ptr.Get(iso));
  Local<ArrayBuffer> buffer = ArrayBuffer::New(iso, static_cast<size_t>(length));
  if (length > 0) {
    std::memcpy(buffer->Data(), data, static_cast<size_t>(length));
  }
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, buffer);
  return tracked_value(ctx, val);
}

// A SharedArrayBuffer in ctx's isolate over a backing store taken from
// another isolate's buffer: the two then address the same bytes, which is
// what posting shared memory to a worker means. The store's shared_ptr is
// copied, so the caller may release its proxy afterwards.
ValuePtr NewSharedArrayBufferFromBackingStore(ContextPtr ctx, BackingStorePtr store) {
  LOCAL_CONTEXT(ctx);
  Local<SharedArrayBuffer> buffer = SharedArrayBuffer::New(iso, store->backing_store);
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, buffer);
  return tracked_value(ctx, val);
}

/********** WebAssembly.Module ***********/

// A compiled module held apart from any context: the code a
// WebAssembly.Module stands for, which a module object in another context --
// or another isolate -- can be built over without compiling again. That is
// what posting a module means; a clone of the object would be a copy of
// nothing, since its state is not in its properties.
struct v8CompiledWasmModule {
  explicit v8CompiledWasmModule(CompiledWasmModule&& compiled)
      : module{std::move(compiled)} {}
  CompiledWasmModule module;
};

CompiledWasmModulePtr WasmModuleObjectGetCompiledModule(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  if (!value->IsWasmModuleObject()) {
    return nullptr;
  }
  return new v8CompiledWasmModule(
      value.As<WasmModuleObject>()->GetCompiledModule());
}

ValuePtr NewWasmModuleObjectFromCompiled(ContextPtr ctx,
                                         CompiledWasmModulePtr mod) {
  LOCAL_CONTEXT(ctx);
  Local<WasmModuleObject> module;
  if (!WasmModuleObject::FromCompiledModule(iso, mod->module)
           .ToLocal(&module)) {
    return nullptr;
  }
  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, module);
  return tracked_value(ctx, val);
}

void CompiledWasmModuleRelease(CompiledWasmModulePtr mod) {
  delete mod;
}

/********** Weak handles **********/

// A weak m_value: V8 may collect the object once nothing in JS reaches it,
// and when it does the first-pass weak callback resets the Global (which V8
// requires of it) and tells Go, by the m_value's address, so the embedder
// can drop its own bookkeeping and free the m_value. The callback runs on
// the isolate's thread inside the collection, so the Go side must not
// allocate V8 handles there; releasing the m_value (ValueRelease: an erase
// and a delete on an already-reset Global) is fine.
static void weakValueCallback(const WeakCallbackInfo<m_value>& info) {
  m_value* val = info.GetParameter();
  if (val == nullptr) {
    return;
  }
  val->ptr.Reset();
  goWeakCallback(static_cast<void*>(val));
}

void ValueSetWeak(ValuePtr ptr) {
  if (ptr == nullptr || ptr->ptr.IsEmpty()) {
    return;
  }
  ptr->ptr.SetWeak(ptr, weakValueCallback, WeakCallbackType::kParameter);
}

void ValueClearWeak(ValuePtr ptr) {
  if (ptr == nullptr || ptr->ptr.IsEmpty()) {
    return;
  }
  ptr->ptr.ClearWeak();
}

// A full garbage collection now (Mark-Compact), the way V8 answers a
// low-memory signal. What a test that asserts collection needs, without the
// --expose-gc flag and its `gc` global.
void IsolateLowMemoryNotification(IsolatePtr iso) {
  ISOLATE_SCOPE(iso);
  iso->LowMemoryNotification();
}

// WeakRef.deref() keeps its target alive until the end of the current job;
// the embedder marks that end by calling this, after each microtask
// checkpoint. An embedder that never does keeps every deref'd object alive
// for the life of the isolate, and a WeakRef is then a strong reference
// with extra steps.
void IsolateClearKeptObjects(IsolatePtr iso) {
  ISOLATE_SCOPE(iso);
  iso->ClearKeptObjects();
}

void BackingStoreRelease(BackingStorePtr ptr) {
  if (ptr == nullptr) {
    return;
  }
  ptr->backing_store.reset();
  delete ptr;
}

void* BackingStoreData(BackingStorePtr ptr) {
  if (ptr == nullptr) {
    return nullptr;
  }

  return ptr->backing_store->Data();
}

size_t BackingStoreByteLength(BackingStorePtr ptr) {
  if (ptr == nullptr) {
    return 0;
  }
  return ptr->backing_store->ByteLength();
}
}

// ---------------------------------------------------------------------------
// v8-inspector
//
// The inspector is compiled into libv8 already; what was missing was any way
// to reach it from Go. The shape follows the rest of this file: an opaque
// struct per object, a uintptr ref back into Go for callbacks, UTF-8 at the
// boundary in both directions.
//
// The one genuinely tricky piece is pausing. When the debugger hits a
// breakpoint, V8 does not return: it calls the client's runMessageLoopOnPause
// and expects the embedder to keep feeding it protocol messages ON THE SAME
// THREAD until one of them resumes execution. So the loop here calls back
// into Go once per tick, Go blocks on its message queue, dispatches whatever
// arrives back into the session (re-entrantly - that is the designed use),
// and quitMessageLoopOnPause flips the flag the loop is standing on.

namespace {

// viewToUtf8 converts a StringView's contents to UTF-8, whichever width the
// buffer is. An 8-bit view is Latin-1 by V8's convention, not UTF-8, so its
// high half encodes to two bytes.
std::string viewToUtf8(const v8_inspector::StringView& view) {
  std::string out;
  if (view.is8Bit()) {
    const uint8_t* chars = view.characters8();
    out.reserve(view.length());
    for (size_t i = 0; i < view.length(); i++) {
      uint8_t c = chars[i];
      if (c < 0x80) {
        out.push_back(static_cast<char>(c));
      } else {
        out.push_back(static_cast<char>(0xC0 | (c >> 6)));
        out.push_back(static_cast<char>(0x80 | (c & 0x3F)));
      }
    }
    return out;
  }
  const uint16_t* chars = view.characters16();
  out.reserve(view.length());
  for (size_t i = 0; i < view.length(); i++) {
    uint32_t code = chars[i];
    if (code >= 0xD800 && code <= 0xDBFF && i + 1 < view.length() &&
        chars[i + 1] >= 0xDC00 && chars[i + 1] <= 0xDFFF) {
      code = 0x10000 + ((code - 0xD800) << 10) + (chars[i + 1] - 0xDC00);
      i++;
    }
    if (code < 0x80) {
      out.push_back(static_cast<char>(code));
    } else if (code < 0x800) {
      out.push_back(static_cast<char>(0xC0 | (code >> 6)));
      out.push_back(static_cast<char>(0x80 | (code & 0x3F)));
    } else if (code < 0x10000) {
      out.push_back(static_cast<char>(0xE0 | (code >> 12)));
      out.push_back(static_cast<char>(0x80 | ((code >> 6) & 0x3F)));
      out.push_back(static_cast<char>(0x80 | (code & 0x3F)));
    } else {
      out.push_back(static_cast<char>(0xF0 | (code >> 18)));
      out.push_back(static_cast<char>(0x80 | ((code >> 12) & 0x3F)));
      out.push_back(static_cast<char>(0x80 | ((code >> 6) & 0x3F)));
      out.push_back(static_cast<char>(0x80 | (code & 0x3F)));
    }
  }
  return out;
}

// utf8ToUtf16 is the other direction: the frontend's JSON arrives from Go as
// UTF-8 and the session wants a StringView, whose 8-bit flavour is Latin-1 -
// so anything beyond ASCII must go through the 16-bit one.
std::vector<uint16_t> utf8ToUtf16(const char* data, int length) {
  std::vector<uint16_t> out;
  out.reserve(length);
  for (int i = 0; i < length;) {
    uint8_t c = static_cast<uint8_t>(data[i]);
    uint32_t code = 0xFFFD;
    int size = 1;
    if (c < 0x80) {
      code = c;
    } else if ((c & 0xE0) == 0xC0 && i + 1 < length) {
      code = ((c & 0x1F) << 6) | (data[i + 1] & 0x3F);
      size = 2;
    } else if ((c & 0xF0) == 0xE0 && i + 2 < length) {
      code = ((c & 0x0F) << 12) | ((data[i + 1] & 0x3F) << 6) |
             (data[i + 2] & 0x3F);
      size = 3;
    } else if ((c & 0xF8) == 0xF0 && i + 3 < length) {
      code = ((c & 0x07) << 18) | ((data[i + 1] & 0x3F) << 12) |
             ((data[i + 2] & 0x3F) << 6) | (data[i + 3] & 0x3F);
      size = 4;
    }
    if (code >= 0x10000) {
      code -= 0x10000;
      out.push_back(static_cast<uint16_t>(0xD800 + (code >> 10)));
      out.push_back(static_cast<uint16_t>(0xDC00 + (code & 0x3FF)));
    } else {
      out.push_back(static_cast<uint16_t>(code));
    }
    i += size;
  }
  return out;
}

class InspectorGoClient;

}  // namespace

// m_inspector owns the V8Inspector and the client V8 calls back into. One per
// isolate is the arrangement; the context group id is fixed at 1 because a
// group is a frame tree and this embedder gives each context its own session.
struct m_inspector {
  Isolate* iso;
  uintptr_t ref;  // the Go handle callbacks identify themselves with
  std::unique_ptr<v8_inspector::V8InspectorClient> client;
  std::unique_ptr<v8_inspector::V8Inspector> inspector;
  bool paused = false;
};

namespace {

class InspectorGoClient : public v8_inspector::V8InspectorClient {
 public:
  explicit InspectorGoClient(m_inspector* owner) : owner_(owner) {}

  void runMessageLoopOnPause(int) override {
    owner_->paused = true;
    while (owner_->paused) {
      // Go blocks until the frontend sends something, dispatches it into the
      // session (on this thread, which is the rule), and returns. A resume or
      // step lands in quitMessageLoopOnPause below before the next tick.
      if (!goInspectorPauseTick(owner_->ref)) {
        break;  // the Go side is shutting the session down
      }
    }
    owner_->paused = false;
  }

  void quitMessageLoopOnPause() override { owner_->paused = false; }

  Local<Context> ensureDefaultContextInGroup(int) override {
    m_ctx* ctx = isolateInternalContext(owner_->iso);
    if (ctx == nullptr) {
      return Local<Context>();
    }
    return ctx->ptr.Get(owner_->iso);
  }

 private:
  m_inspector* owner_;
};

class InspectorGoChannel : public v8_inspector::V8Inspector::Channel {
 public:
  explicit InspectorGoChannel(uintptr_t ref) : ref_(ref) {}

  void sendResponse(
      int callId, std::unique_ptr<v8_inspector::StringBuffer> message) override {
    forward(callId, std::move(message));
  }
  void sendNotification(
      std::unique_ptr<v8_inspector::StringBuffer> message) override {
    forward(-1, std::move(message));
  }
  void flushProtocolNotifications() override {}

 private:
  void forward(int callId,
               std::unique_ptr<v8_inspector::StringBuffer> message) {
    std::string utf8 = viewToUtf8(message->string());
    goInspectorMessage(ref_, callId, const_cast<char*>(utf8.data()),
                       static_cast<int>(utf8.size()));
  }
  uintptr_t ref_;
};

}  // namespace

// m_inspectorSession is one protocol connection: the channel Go messages leave
// through and the session they arrive into.
struct m_inspectorSession {
  m_inspector* inspector;
  std::unique_ptr<InspectorGoChannel> channel;
  std::unique_ptr<v8_inspector::V8InspectorSession> session;
};

extern "C" {

InspectorPtr NewInspector(IsolatePtr iso, uintptr_t ref) {
  ISOLATE_SCOPE(iso);
  m_inspector* insp = new m_inspector;
  insp->iso = iso;
  insp->ref = ref;
  insp->client = std::make_unique<InspectorGoClient>(insp);
  insp->inspector = v8_inspector::V8Inspector::create(iso, insp->client.get());
  return insp;
}

void InspectorContextCreated(InspectorPtr insp, ContextPtr ctx) {
  ISOLATE_SCOPE(insp->iso);
  Local<Context> local = ctx->ptr.Get(insp->iso);
  v8_inspector::StringView name(reinterpret_cast<const uint8_t*>("page"), 4);
  insp->inspector->contextCreated(v8_inspector::V8ContextInfo(local, 1, name));
}

void InspectorContextDestroyed(InspectorPtr insp, ContextPtr ctx) {
  ISOLATE_SCOPE(insp->iso);
  insp->inspector->contextDestroyed(ctx->ptr.Get(insp->iso));
}

void InspectorDispose(InspectorPtr insp) {
  {
    ISOLATE_SCOPE(insp->iso);
    insp->inspector.reset();
    insp->client.reset();
  }
  delete insp;
}

InspectorSessionPtr InspectorConnect(InspectorPtr insp, uintptr_t ref) {
  ISOLATE_SCOPE(insp->iso);
  m_inspectorSession* s = new m_inspectorSession;
  s->inspector = insp;
  s->channel = std::make_unique<InspectorGoChannel>(ref);
  s->session = insp->inspector->connect(
      1, s->channel.get(), v8_inspector::StringView(),
      v8_inspector::V8Inspector::kFullyTrusted,
      v8_inspector::V8Inspector::kNotWaitingForDebugger);
  return s;
}

// PumpPlatformTasks runs the platform's queued foreground tasks for the
// isolate until there are none. Modern V8 defers several inspector commands
// through here — HeapProfiler.takeHeapSnapshot posts the snapshot as a task
// and answers only when it has run — and an embedder that never pumps sees
// those commands simply vanish: the enable round-trips, the snapshot never
// answers and never sends a chunk.
void PumpPlatformTasks(IsolatePtr iso) {
  ISOLATE_SCOPE(iso);
  while (platform::PumpMessageLoop(default_platform.get(), iso)) {
  }
}

void InspectorSessionDispatch(InspectorSessionPtr s, const char* message,
                              int length) {
  Isolate* iso = s->inspector->iso;
  ISOLATE_SCOPE(iso);
  std::vector<uint16_t> wide = utf8ToUtf16(message, length);
  s->session->dispatchProtocolMessage(
      v8_inspector::StringView(wide.data(), wide.size()));
  // Some commands answer through a platform task rather than synchronously -
  // HeapProfiler.takeHeapSnapshot posts the snapshot and responds only when
  // it has run. Nothing else in this embedder pumps the platform, so the
  // dispatch drains it before returning: the reply and its chunks arrive
  // through the channel like any synchronous answer.
  PumpPlatformTasks(iso);
}

void InspectorSessionDispose(InspectorSessionPtr s) {
  {
    ISOLATE_SCOPE(s->inspector->iso);
    s->session.reset();
    s->channel.reset();
  }
  delete s;
}

}  // extern "C"
