package v8go

// #include <stdlib.h>
// #include "v8go.h"
import "C"
import (
	"runtime"
	"unsafe"
)

// The lean callback ABI.
//
// A FunctionCallback costs the embedder more than its own work. The C++ side
// mints a tracked handle — a Global<Value> plus a context map entry — for the
// receiver and for every argument; reading an argument's string is a crossing
// back into C++, releasing each handle is another, and a string result is
// minted as one more handle for the shim to free. A DOM getter that answers a
// string from a map lookup paid six or seven crossings to do it.
//
// A FastCallback crosses once. Its receiver arrives as internal field 0 (the
// id an embedder keys its native object by), its leading arguments decoded on
// the C++ side into plain values according to the template's spec, and its
// result goes back through a slot the C++ side reads when the callback has
// returned — a primitive or a string by value, or a handle the Go side already
// holds. Nothing is minted for the call and nothing is left to release.
//
// What it cannot do is hand the callback a JS object: an object argument
// arrives as its internal field (FastWrapper) or as the bare fact that it is
// an object (FastObject). A binding that needs the object itself stays on
// NewFunctionTemplate.

// FastKind says what an argument was, before any spec conversion.
type FastKind int

const (
	FastAbsent    FastKind = C.FastAbsent
	FastUndefined FastKind = C.FastUndefined
	FastNull      FastKind = C.FastNull
	FastBool      FastKind = C.FastBool
	FastInt32     FastKind = C.FastInt32
	FastNumber    FastKind = C.FastNumber
	FastString    FastKind = C.FastString
	FastObject    FastKind = C.FastObject
	FastWrapper   FastKind = C.FastWrapper
	FastOther     FastKind = C.FastOther
)

// FastMaxArgs is how many leading arguments a spec can decode.
const FastMaxArgs = C.kFastMaxArgs

// FastCallback is the callback a fast FunctionTemplate runs.
type FastCallback func(info *FastCallbackInfo)

// FastCallbackInfo is one fast call: the receiver's field, the decoded
// arguments, and the result the callback sets. It is valid only during the
// call.
type FastCallbackInfo struct {
	ctx    *Context
	field0 int64
	argc   int
	args   *[C.kFastMaxArgs]C.FastArg
	ret    *C.FastRet
	// retString is copied into the C++ side's buffer after the callback has
	// returned, not when it is set: the callback may run script after
	// setting it, and a nested fast call shares the buffer.
	retString string
	hasString bool
}

// Context is the context the call is running in.
func (i *FastCallbackInfo) Context() *Context { return i.ctx }

// ThisInternalField is the receiver's internal field 0 when it holds an
// int32, as FunctionCallbackInfo.ThisInternalField.
func (i *FastCallbackInfo) ThisInternalField() (int64, bool) {
	if i.field0 == noInternalField {
		return 0, false
	}
	return i.field0, true
}

// Len is the number of arguments the call was made with, decoded or not.
func (i *FastCallbackInfo) Len() int { return i.argc }

func (i *FastCallbackInfo) arg(n int) *C.FastArg {
	if n < 0 || n >= FastMaxArgs || n >= i.argc {
		return nil
	}
	return &i.args[n]
}

// Kind is argument n's kind; FastAbsent past the arguments given or the spec.
func (i *FastCallbackInfo) Kind(n int) FastKind {
	a := i.arg(n)
	if a == nil {
		return FastAbsent
	}
	return FastKind(a.kind)
}

// String is argument n as a string: the string itself, or with an 's' spec
// the ToString of whatever it was. Empty for an argument with no string.
func (i *FastCallbackInfo) String(n int) string {
	a := i.arg(n)
	if a == nil || a.str == nil {
		return ""
	}
	return C.GoStringN(a.str, a.len)
}

// Int32 is argument n as an int32 (a boolean as 0/1); ok is false for any
// other kind.
func (i *FastCallbackInfo) Int32(n int) (int32, bool) {
	a := i.arg(n)
	if a == nil || (a.kind != C.FastInt32 && a.kind != C.FastBool) {
		return 0, false
	}
	return int32(a.i32), true
}

// Number is argument n as a float64 when it was a number.
func (i *FastCallbackInfo) Number(n int) (float64, bool) {
	a := i.arg(n)
	if a == nil || (a.kind != C.FastInt32 && a.kind != C.FastNumber) {
		return 0, false
	}
	return float64(a.f64), true
}

// Bool is argument n when it was a boolean.
func (i *FastCallbackInfo) Bool(n int) (bool, bool) {
	a := i.arg(n)
	if a == nil || a.kind != C.FastBool {
		return false, false
	}
	return a.i32 != 0, true
}

// WrapperField is argument n's internal field 0 when it was an object
// carrying an int32 one.
func (i *FastCallbackInfo) WrapperField(n int) (int64, bool) {
	a := i.arg(n)
	if a == nil || a.kind != C.FastWrapper {
		return 0, false
	}
	return int64(a.i32), true
}

// ReturnUndefined, ReturnNull, ReturnBool, ReturnInt32, ReturnNumber and
// ReturnString set the result by value.
func (i *FastCallbackInfo) ReturnUndefined() { i.ret.kind = C.FastRetUndefined; i.hasString = false }
func (i *FastCallbackInfo) ReturnNull()      { i.ret.kind = C.FastRetNull; i.hasString = false }
func (i *FastCallbackInfo) ReturnBool(b bool) {
	i.ret.kind = C.FastRetBool
	i.ret.i32 = 0
	if b {
		i.ret.i32 = 1
	}
	i.hasString = false
}
func (i *FastCallbackInfo) ReturnInt32(n int32) {
	i.ret.kind = C.FastRetInt32
	i.ret.i32 = C.int32_t(n)
	i.hasString = false
}
func (i *FastCallbackInfo) ReturnNumber(f float64) {
	i.ret.kind = C.FastRetNumber
	i.ret.f64 = C.double(f)
	i.hasString = false
}
func (i *FastCallbackInfo) ReturnString(s string) {
	i.ret.kind = C.FastRetString
	i.retString, i.hasString = s, true
}

// ReturnValue sets a handle the caller keeps — a cached wrapper, an atom.
// It is not released by the call, and must outlive it.
func (i *FastCallbackInfo) ReturnValue(v *Value) {
	i.hasString = false
	if v == nil || v.ptr == nil {
		i.ret.kind = C.FastRetUndefined
		return
	}
	i.ret.kind = C.FastRetValue
	i.ret.value = v.ptr
}

// ReturnOwnedValue sets a handle the callback minted; the call frees it once
// V8 has the value. The caller must not use or release v afterwards.
func (i *FastCallbackInfo) ReturnOwnedValue(v *Value) {
	i.hasString = false
	if v == nil || v.ptr == nil {
		i.ret.kind = C.FastRetUndefined
		return
	}
	i.ret.kind = C.FastRetOwnedValue
	i.ret.value = v.ptr
}

// NewFastFunctionTemplate makes a FunctionTemplate whose calls take the lean
// ABI. spec has one letter per leading argument to decode ('v' as it is,
// 's' converted to a string), at most FastMaxArgs.
func NewFastFunctionTemplate(iso *Isolate, spec string, callback FastCallback) *FunctionTemplate {
	if iso == nil {
		panic("nil Isolate argument not supported")
	}
	if callback == nil {
		panic("nil FastCallback argument not supported")
	}
	if len(spec) > FastMaxArgs {
		panic("v8go: a fast spec decodes at most FastMaxArgs arguments")
	}
	for _, c := range spec {
		if c != 'v' && c != 's' {
			panic("v8go: a fast spec letter is 'v' or 's'")
		}
	}
	cbref := iso.registerFastCallback(callback)
	cspec := C.CString(spec)
	defer C.free(unsafe.Pointer(cspec))
	tmpl := &template{
		ptr:   C.NewFastFunctionTemplate(iso.ptr, C.int(cbref), cspec),
		iso:   iso,
		cbref: cbref,
	}
	runtime.SetFinalizer(tmpl, (*template).finalizer)
	return &FunctionTemplate{tmpl}
}

//export goFastCallback
func goFastCallback(ctxref C.int, cbref C.int, field0 C.int64_t, argc C.int, args *C.FastArg, ret *C.FastRet) {
	ctx := getContext(int(ctxref))
	if ctx == nil {
		return
	}
	cb := ctx.iso.getFastCallback(int(cbref))
	if cb == nil {
		return
	}
	// The info comes from a per-isolate stack rather than the heap: a fast
	// call allocates nothing of its own. A callback that runs script can be
	// re-entered, which is why it is a stack and not one slot.
	iso := ctx.iso
	if iso.fastDepth == len(iso.fastInfos) {
		iso.fastInfos = append(iso.fastInfos, &FastCallbackInfo{})
	}
	info := iso.fastInfos[iso.fastDepth]
	iso.fastDepth++
	*info = FastCallbackInfo{
		ctx:    ctx,
		field0: int64(field0),
		argc:   int(argc),
		args:   (*[C.kFastMaxArgs]C.FastArg)(unsafe.Pointer(args)),
		ret:    ret,
	}
	cb(info)
	iso.fastDepth--
	s, has := info.retString, info.hasString
	*info = FastCallbackInfo{}
	if !has {
		return
	}
	if len(s) > int(ret.cap) {
		// Longer than the buffer: minted, and freed by the call.
		v, err := NewValue(ctx.iso, s)
		if err != nil {
			ret.kind = C.FastRetUndefined
			return
		}
		ret.kind = C.FastRetOwnedValue
		ret.value = v.ptr
		return
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(ret.buf)), len(s)), s)
	ret.len = C.int(len(s))
}

// NewWrapper instantiates the template with internal field 0 set to field,
// its prototype set to proto (nil keeps the template's), and one own
// property per key/value pair — the whole of making a DOM wrapper, in one
// crossing. proto, keys and vals are borrowed.
func (o *ObjectTemplate) NewWrapper(ctx *Context, field int32, proto *Value, keys, vals []*Value) (*Object, error) {
	return o.newWrapper(ctx, field, proto, keys, vals, nil)
}

// NewWeakWrapper is NewWrapper with the handle born weak and releasing, as
// SetWeakReleasing would leave it, without the second crossing. The caller
// must hand the object to script before anything can run a collection — in
// practice, return it from the callback that made it — or the collection can
// free the handle under it.
func (o *ObjectTemplate) NewWeakWrapper(ctx *Context, field int32, proto *Value, onCollected func()) (*Object, error) {
	return o.newWrapper(ctx, field, proto, nil, nil, onCollected)
}

func (o *ObjectTemplate) newWrapper(ctx *Context, field int32, proto *Value, keys, vals []*Value, onCollected func()) (*Object, error) {
	if len(keys) != len(vals) {
		panic("v8go: NewWrapper needs a value per key")
	}
	var protoPtr C.ValuePtr
	if proto != nil {
		protoPtr = proto.ptr
	}
	var kp, vp *C.ValuePtr
	var kbuf, vbuf [4]C.ValuePtr
	n := len(keys)
	if n > 0 {
		ks, vs := kbuf[:0], vbuf[:0]
		if n > len(kbuf) {
			ks, vs = make([]C.ValuePtr, 0, n), make([]C.ValuePtr, 0, n)
		}
		for i := range keys {
			var k, v C.ValuePtr
			if keys[i] != nil {
				k = keys[i].ptr
			}
			if vals[i] != nil {
				v = vals[i].ptr
			}
			ks, vs = append(ks, k), append(vs, v)
		}
		kp, vp = &ks[0], &vs[0]
	}
	weak := C.int(0)
	if onCollected != nil {
		weak = 1
	}
	rtn := C.ObjectTemplateNewWrapper(o.ptr, ctx.ptr, C.int32_t(field), protoPtr, C.int(n), kp, vp, weak)
	runtime.KeepAlive(o)
	runtime.KeepAlive(proto)
	runtime.KeepAlive(keys)
	runtime.KeepAlive(vals)
	val, err := valueResult(ctx, rtn)
	if err != nil {
		return nil, err
	}
	if onCollected != nil {
		// No V8 work happens between the handle going weak and this, so the
		// callback is filed before any collection can want it.
		weakHandlers.Lock()
		weakHandlers.byPtr[unsafe.Pointer(val.ptr)] = onCollected
		weakHandlers.Unlock()
	}
	return &Object{val}, nil
}

// NewArrayOf builds a JS array of the given handles in one crossing; a nil
// entry is undefined. The handles are borrowed.
func NewArrayOf(ctx *Context, vals []*Value) (*Value, error) {
	var ptrs []C.ValuePtr
	var buf [16]C.ValuePtr
	if len(vals) <= len(buf) {
		ptrs = buf[:len(vals)]
	} else {
		ptrs = make([]C.ValuePtr, len(vals))
	}
	for i, v := range vals {
		if v != nil {
			ptrs[i] = v.ptr
		} else {
			ptrs[i] = nil
		}
	}
	var p *C.ValuePtr
	if len(ptrs) > 0 {
		p = &ptrs[0]
	}
	rtn := C.NewArrayOfValues(ctx.ptr, C.int(len(vals)), p)
	runtime.KeepAlive(vals)
	return valueResult(ctx, rtn)
}

// SetWeakAll is SetWeak for several handles in one crossing, each with its
// own callback.
func SetWeakAll(vals []*Value, onCollected []func()) {
	setWeakAll(vals, onCollected, false)
}

// SetWeakAllReleasing is SetWeakAll for handles the caller gives up: each
// one is freed once its callback has run, so the callback must not Release
// it — the Release crossing is the one this saves. ClearWeak takes a handle
// back as usual, and a handle taken back is the caller's to release again.
func SetWeakAllReleasing(vals []*Value, onCollected []func()) {
	setWeakAll(vals, onCollected, true)
}

// SetWeakReleasing is SetWeakAllReleasing for one handle.
func (v *Value) SetWeakReleasing(onCollected func()) {
	if v == nil || v.ptr == nil || onCollected == nil {
		return
	}
	weakHandlers.Lock()
	weakHandlers.byPtr[unsafe.Pointer(v.ptr)] = onCollected
	weakHandlers.Unlock()
	C.ValueSetWeakReleasing(v.ptr)
}

func setWeakAll(vals []*Value, onCollected []func(), releasing bool) {
	if len(vals) != len(onCollected) {
		panic("v8go: SetWeakAll needs a callback per value")
	}
	ptrs := make([]C.ValuePtr, 0, len(vals))
	weakHandlers.Lock()
	for i, v := range vals {
		if v == nil || v.ptr == nil || onCollected[i] == nil {
			continue
		}
		weakHandlers.byPtr[unsafe.Pointer(v.ptr)] = onCollected[i]
		ptrs = append(ptrs, v.ptr)
	}
	weakHandlers.Unlock()
	if len(ptrs) == 0 {
		return
	}
	r := C.int(0)
	if releasing {
		r = 1
	}
	C.ValuesSetWeak(C.int(len(ptrs)), &ptrs[0], r)
	runtime.KeepAlive(vals)
}
