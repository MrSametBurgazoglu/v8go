// Copyright 2019 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

#include "v8go.h"

#include <stdio.h>

#include <cstdlib>
#include <cstring>
#include <iostream>
#include <sstream>
#include <string>
#include <unordered_map>
#include <vector>

#include "_cgo_export.h"

using namespace v8;

auto default_platform = platform::NewDefaultPlatform();
ArrayBuffer::Allocator* default_allocator;

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
};

struct m_value {
  long id;
  Isolate* iso;
  m_ctx* ctx;
  Global<Value> ptr;
};

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

  Local<Message> msg = try_catch.Message();
  if (!msg.IsEmpty()) {
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

  default_allocator = ArrayBuffer::Allocator::NewDefaultAllocator();
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

void IsolateDispose(IsolatePtr iso) {
  if (iso == nullptr) {
    return;
  }
  ContextFree(isolateInternalContext(iso));

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

  m_value* val = new m_value;
  val->id = 0;
  val->iso = iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(iso, msg.GetValue());

  goPromiseRejectCallback(iso, static_cast<int>(msg.GetEvent()), val);

  val->ptr.Reset();
  delete val;
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
  if (!String::NewFromUtf8(iso, it->second.c_str(), NewStringType::kNormal)
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
  return compileRegistryModule(context, specifier);
}

// hostImportModuleDynamically is the isolate-level host hook fired by
// `await import(specifier)`. For an in-memory registry the module resolves
// synchronously: compile -> instantiate -> evaluate -> resolve the returned
// promise with the module namespace.
static MaybeLocal<Promise> hostImportModuleDynamically(
    Local<Context> context, Local<Data> /*host_defined_options*/,
    Local<Value> resource_name, Local<String> specifier,
    Local<FixedArray> /*import_attributes*/) {
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
    resolver->Reject(context, Exception::Error(msg)).Check();
    return resolver->GetPromise();
  }

  Local<Module> module;
  if (!compileRegistryModule(context, specifier).ToLocal(&module)) {
    resolver->Reject(context, try_catch.Exception()).Check();
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
      resolver->Reject(context, try_catch.Exception()).Check();
      return resolver->GetPromise();
    }
    status = module->GetStatus();
  }

  // Evaluate returns a Promise per spec. A module without top-level await
  // settles it synchronously; a module-level throw lands in kErrored (not in
  // try_catch), so the status must be inspected explicitly.
  if (status == Module::kInstantiated) {
    if (module->Evaluate(context).IsEmpty()) {
      resolver->Reject(context, try_catch.Exception()).Check();
      return resolver->GetPromise();
    }
    if (module->GetStatus() == Module::kErrored) {
      resolver->Reject(context, module->GetException()).Check();
      return resolver->GetPromise();
    }
  }

  resolver->Resolve(context, module->GetModuleNamespace()).Check();
  return resolver->GetPromise();
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
}

// ContextRegisterModule stores ES module source under a specifier string so
// the dynamic-import host hook can compile it. Registered on the context; the
// hook recovers the same context via the ctx_ref embedder slot.
void ContextRegisterModule(ContextPtr ctx, const char* specifier,
                           const char* source) {
  ctx->modules[specifier] = source;
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
                                             const char* o,
                                             CompileOptions opts) {
  ISOLATE_SCOPE_INTERNAL_CONTEXT(iso);
  TryCatch try_catch(iso);
  Local<Context> local_ctx = ctx->ptr.Get(iso);
  Context::Scope context_scope(local_ctx);

  RtnUnboundScript rtn = {};

  Local<String> src =
      String::NewFromUtf8(iso, s, NewStringType::kNormal).ToLocalChecked();
  Local<String> ogn =
      String::NewFromUtf8(iso, o, NewStringType::kNormal).ToLocalChecked();

  ScriptCompiler::CompileOptions option =
      static_cast<ScriptCompiler::CompileOptions>(opts.compileOption);

  ScriptCompiler::CachedData* cached_data = nullptr;

  if (opts.cachedData.data) {
    cached_data = new ScriptCompiler::CachedData(opts.cachedData.data,
                                                 opts.cachedData.length);
  }

  ScriptOrigin script_origin(ogn);

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

void TemplateSetValue(TemplatePtr ptr,
                      const char* name,
                      ValuePtr val,
                      int attributes) {
  LOCAL_TEMPLATE(ptr);

  Local<String> prop_name =
      String::NewFromUtf8(iso, name, NewStringType::kNormal).ToLocalChecked();
  tmpl->Set(prop_name, val->ptr.Get(iso), (PropertyAttribute)attributes);
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

/********** FunctionTemplate **********/

static void FunctionTemplateCallback(const FunctionCallbackInfo<Value>& info) {
  Isolate* iso = info.GetIsolate();
  ISOLATE_SCOPE(iso);

  // This callback function can be called from any Context, which we only know
  // at runtime. We extract the Context reference from the embedder data so that
  // we can use the context registry to match the Context on the Go side
  Local<Context> local_ctx = iso->GetCurrentContext();
  int ctx_ref = local_ctx->GetEmbedderData(1).As<Integer>()->Value();
  m_ctx* ctx = goContext(ctx_ref);

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

  ValuePtr val =
      goFunctionCallback(ctx_ref, callback_ref, thisAndArgs, args_count);
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

/********** Context **********/

#define LOCAL_CONTEXT(ctx)                      \
  Isolate* iso = ctx->iso;                      \
  Locker locker(iso);                           \
  Isolate::Scope isolate_scope(iso);            \
  HandleScope handle_scope(iso);                \
  TryCatch try_catch(iso);                      \
  Local<Context> local_ctx = ctx->ptr.Get(iso); \
  Context::Scope context_scope(local_ctx);

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
  Local<Context> local_ctx = Context::New(iso, nullptr, global_template);
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

  delete ctx;
}

RtnValue RunScript(ContextPtr ctx, const char* source, const char* origin) {
  LOCAL_CONTEXT(ctx);

  RtnValue rtn = {};

  MaybeLocal<String> maybeSrc =
      String::NewFromUtf8(iso, source, NewStringType::kNormal);
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
  return value->Int32Value(local_ctx).ToChecked();
}

int64_t ValueToInteger(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->IntegerValue(local_ctx).ToChecked();
}

double ValueToNumber(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->NumberValue(local_ctx).ToChecked();
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

uint32_t ValueToUint32(ValuePtr ptr) {
  LOCAL_VALUE(ptr);
  return value->Uint32Value(local_ctx).ToChecked();
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
  return obj->Has(local_ctx, key_val).ToChecked();
}

int ObjectHasIdx(ValuePtr ptr, uint32_t idx) {
  LOCAL_OBJECT(ptr);
  return obj->Has(local_ctx, idx).ToChecked();
}

int ObjectDelete(ValuePtr ptr, const char* key) {
  LOCAL_OBJECT(ptr);
  Local<String> key_val =
      String::NewFromUtf8(iso, key, NewStringType::kNormal).ToLocalChecked();
  return obj->Delete(local_ctx, key_val).ToChecked();
}

int ObjectDeleteIdx(ValuePtr ptr, uint32_t idx) {
  LOCAL_OBJECT(ptr);
  return obj->Delete(local_ctx, idx).ToChecked();
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
  return resolver->Resolve(local_ctx, resolve_val->ptr.Get(iso)).ToChecked();
}

int PromiseResolverReject(ValuePtr ptr, ValuePtr reject_val) {
  LOCAL_VALUE(ptr);
  Local<Promise::Resolver> resolver = value.As<Promise::Resolver>();
  return resolver->Reject(local_ctx, reject_val->ptr.Get(iso)).ToChecked();
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

/********** v8::V8 **********/

const char* Version() {
  return V8::GetVersion();
}

void SetFlags(const char* flags) {
  V8::SetFlagsFromString(flags);
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
