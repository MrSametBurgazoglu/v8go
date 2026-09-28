package v8go_test

import (
	"fmt"
	"strings"
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// fastFixture is a context whose global has `make(id)` — an object carrying
// id in internal field 0 — and a fast function `f` built from spec and cb.
func fastFixture(t testing.TB, spec string, cb v8.FastCallback) (*v8.Isolate, *v8.Context) {
	t.Helper()
	iso := v8.NewIsolate()
	global := v8.NewObjectTemplate(iso)
	wrapper := v8.NewObjectTemplate(iso)
	wrapper.SetInternalFieldCount(1)
	var ctx *v8.Context
	if err := global.Set("f", v8.NewFastFunctionTemplate(iso, spec, cb)); err != nil {
		t.Fatal(err)
	}
	if err := global.Set("make", v8.NewFunctionTemplate(iso, func(info *v8.FunctionCallbackInfo) *v8.Value {
		obj, err := wrapper.NewWrapper(ctx, info.Args()[0].Int32(), nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return obj.Value
	})); err != nil {
		t.Fatal(err)
	}
	ctx = v8.NewContext(iso, global)
	t.Cleanup(func() { ctx.Close(); iso.Dispose() })
	return iso, ctx
}

func run(t testing.TB, ctx *v8.Context, src string) *v8.Value {
	t.Helper()
	v, err := ctx.RunScript(src, "fast.js")
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	return v
}

func TestFastCallbackDecodesArguments(t *testing.T) {
	var got []string
	_, ctx := fastFixture(t, "vvvv", func(info *v8.FastCallbackInfo) {
		for i := 0; i < 4; i++ {
			switch k := info.Kind(i); k {
			case v8.FastString:
				got = append(got, "s:"+info.String(i))
			case v8.FastInt32:
				n, _ := info.Int32(i)
				got = append(got, fmt.Sprint("i:", n))
			case v8.FastNumber:
				f, _ := info.Number(i)
				got = append(got, fmt.Sprint("n:", f))
			case v8.FastBool:
				b, _ := info.Bool(i)
				got = append(got, fmt.Sprint("b:", b))
			case v8.FastWrapper:
				id, _ := info.WrapperField(i)
				got = append(got, fmt.Sprint("w:", id))
			default:
				got = append(got, fmt.Sprint("k:", int(k)))
			}
		}
		got = append(got, fmt.Sprint("len:", info.Len()))
	})
	run(t, ctx, `f('héllo', 7, 1.5, true); f(make(42), {}, null, undefined, 'extra'); f(); f(Symbol(), 1n)`)
	want := "s:héllo i:7 n:1.5 b:true len:4 w:42 k:7 k:2 k:1 len:5 k:0 k:0 k:0 k:0 len:0 k:9 k:9 k:0 k:0 len:2"
	if s := strings.Join(got, " "); s != want {
		t.Errorf("decoded\n %s\nwant\n %s", s, want)
	}
}

func TestFastCallbackConvertsDOMStrings(t *testing.T) {
	var got []string
	_, ctx := fastFixture(t, "ss", func(info *v8.FastCallbackInfo) {
		got = append(got, fmt.Sprintf("%d:%s/%d:%s", info.Kind(0), info.String(0), info.Kind(1), info.String(1)))
	})
	run(t, ctx, `f(null, 12); f({toString() { return 'obj' }}, undefined)`)
	want := "2:null/4:12 7:obj/1:undefined"
	if s := strings.Join(got, " "); s != want {
		t.Errorf("got %s, want %s", s, want)
	}
	// A throwing toString is the caller's exception, and the binding never runs.
	got = nil
	v := run(t, ctx, `try { f({toString() { throw new Error('boom') }}); 'no throw' } catch (e) { e.message }`)
	if v.String() != "boom" || len(got) != 0 {
		t.Errorf("throwing toString: %q, binding ran %d times", v.String(), len(got))
	}
}

func TestFastCallbackReturns(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	kept, _ := v8.NewValue(iso, "kept")
	long := strings.Repeat("x", 70*1024)
	returns := map[string]func(*v8.FastCallbackInfo){
		"undefined": func(i *v8.FastCallbackInfo) { i.ReturnUndefined() },
		"null":      func(i *v8.FastCallbackInfo) { i.ReturnNull() },
		"bool":      func(i *v8.FastCallbackInfo) { i.ReturnBool(true) },
		"int":       func(i *v8.FastCallbackInfo) { i.ReturnInt32(-5) },
		"number":    func(i *v8.FastCallbackInfo) { i.ReturnNumber(2.5) },
		"string":    func(i *v8.FastCallbackInfo) { i.ReturnString("ünïcode") },
		"long":      func(i *v8.FastCallbackInfo) { i.ReturnString(long) },
		"value":     func(i *v8.FastCallbackInfo) { i.ReturnValue(kept) },
		"owned": func(i *v8.FastCallbackInfo) {
			v, _ := v8.NewValue(iso, "minted")
			i.ReturnOwnedValue(v)
		},
		"nothing": func(*v8.FastCallbackInfo) {},
	}
	global := v8.NewObjectTemplate(iso)
	for name, fn := range returns {
		fn := fn
		_ = global.Set("r_"+name, v8.NewFastFunctionTemplate(iso, "", func(i *v8.FastCallbackInfo) { fn(i) }))
	}
	ctx := v8.NewContext(iso, global)
	defer ctx.Close()
	v := run(t, ctx, `JSON.stringify([r_undefined(), r_null(), r_bool(), r_int(), r_number(), r_string(), r_long().length, r_value(), r_value(), r_owned(), r_nothing()].map(x => x === undefined ? 'U' : x))`)
	want := `["U",null,true,-5,2.5,"ünïcode",71680,"kept","kept","minted","U"]`
	if v.String() != want {
		t.Errorf("got %s\nwant %s", v.String(), want)
	}
}

func TestFastCallbackReceiverField(t *testing.T) {
	var fields []string
	iso := v8.NewIsolate()
	defer iso.Dispose()
	global := v8.NewObjectTemplate(iso)
	wrapper := v8.NewObjectTemplate(iso)
	wrapper.SetInternalFieldCount(1)
	_ = global.Set("f", v8.NewFastFunctionTemplate(iso, "", func(i *v8.FastCallbackInfo) {
		id, ok := i.ThisInternalField()
		fields = append(fields, fmt.Sprint(id, ok))
	}))
	ctx := v8.NewContext(iso, global)
	defer ctx.Close()
	obj, err := wrapper.NewWrapper(ctx, 99, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := ctx.Global()
	_ = g.Set("w", obj)
	run(t, ctx, `w.f = f; w.f(); f.call({})`)
	if s := strings.Join(fields, " "); s != "99 true 0 false" {
		t.Errorf("fields %q", s)
	}
}

func TestNewWrapperSetsPrototypeAndProperties(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()
	proto := run(t, ctx, `globalThis.P = { hello() { return 'hi' } }; P`)
	key := run(t, ctx, `globalThis.K = Symbol('k'); K`)
	val, _ := v8.NewValue(iso, int32(3))
	tmpl := v8.NewObjectTemplate(iso)
	tmpl.SetInternalFieldCount(1)
	obj, err := tmpl.NewWrapper(ctx, 7, proto, []*v8.Value{key}, []*v8.Value{val})
	if err != nil {
		t.Fatal(err)
	}
	if id := obj.GetInternalField(0).Int32(); id != 7 {
		t.Errorf("field %d", id)
	}
	g := ctx.Global()
	_ = g.Set("o", obj)
	v := run(t, ctx, `[Object.getPrototypeOf(o) === P, o.hello(), o[K], Object.getOwnPropertyNames(o).length].join()`)
	if v.String() != "true,hi,3,0" {
		t.Errorf("got %s", v.String())
	}
}

func TestTokenWeakHandlesReportTheirTokens(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()
	var got []int64
	v8.SetWeakTokenHandler(func(token int64) { got = append(got, token) })
	defer v8.SetWeakTokenHandler(nil)
	tmpl := v8.NewObjectTemplate(iso)
	tmpl.SetInternalFieldCount(1)
	held, err := tmpl.NewWeakWrapper(ctx, 1, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	g := ctx.Global()
	_ = g.Set("held", held)
	if _, err := tmpl.NewWeakWrapper(ctx, 2, nil, 20); err != nil {
		t.Fatal(err)
	}
	loose := run(t, ctx, `({x: 1})`)
	loose.SetWeakToken(30)
	back := run(t, ctx, `({y: 1})`)
	back.SetWeakToken(40)
	back.ClearWeakToken()
	iso.LowMemoryNotification()
	if len(got) != 2 || got[0]+got[1] != 50 {
		t.Errorf("collected tokens %v, want 20 and 30", got)
	}
	if v := run(t, ctx, `typeof held`).String(); v != "object" {
		t.Errorf("held is %s", v)
	}
	back.Release()
}

func TestNewArrayOf(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()
	a := run(t, ctx, `({a: 1})`)
	b, _ := v8.NewValue(iso, "b")
	many := make([]*v8.Value, 40)
	for i := range many {
		many[i] = a
	}
	arr, err := v8.NewArrayOf(ctx, []*v8.Value{a, nil, b})
	if err != nil {
		t.Fatal(err)
	}
	big, _ := v8.NewArrayOf(ctx, many)
	empty, _ := v8.NewArrayOf(ctx, nil)
	g := ctx.Global()
	_ = g.Set("arr", arr)
	_ = g.Set("big", big)
	_ = g.Set("empty", empty)
	v := run(t, ctx, `[Array.isArray(arr), arr.length, arr[0].a, arr[1], arr[2], big.length, big[39] === arr[0], empty.length].join()`)
	if v.String() != "true,3,1,,b,40,true,0" {
		t.Errorf("got %s", v.String())
	}
}

func TestSetWeakAllCollects(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()
	collected := 0
	var vals []*v8.Value
	var fns []func()
	for i := 0; i < 3; i++ {
		v := run(t, ctx, `({big: new Array(1000).fill(1)})`)
		vals = append(vals, v)
		fns = append(fns, func() { collected++; v.Release() })
	}
	v8.SetWeakAll(vals, fns)
	for _, v := range vals {
		if !v.IsWeak() {
			t.Fatal("not weak")
		}
	}
	iso.LowMemoryNotification()
	if collected != 3 {
		t.Errorf("collected %d of 3", collected)
	}
}

func TestSetWeakAllReleasingFreesTheHandle(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()
	collected := 0
	var vals []*v8.Value
	var fns []func()
	for i := 0; i < 3; i++ {
		vals = append(vals, run(t, ctx, `({big: new Array(1000).fill(1)})`))
		fns = append(fns, func() { collected++ })
	}
	// One taken back before the collection: it survives and stays ours.
	kept := run(t, ctx, `globalThis.keep = {k: 1}; keep`)
	v8.SetWeakAllReleasing(append(vals, kept), append(fns, func() { collected += 100 }))
	kept.ClearWeak()
	iso.LowMemoryNotification()
	if collected != 3 {
		t.Errorf("collected %d, want 3", collected)
	}
	kept.Release()
}

func TestFastTemplateRelease(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	before := iso.CallbackCount()
	tmpl := v8.NewFastFunctionTemplate(iso, "s", func(*v8.FastCallbackInfo) {})
	if iso.CallbackCount() != before+1 {
		t.Fatalf("count %d, want %d", iso.CallbackCount(), before+1)
	}
	tmpl.Release()
	if iso.CallbackCount() != before {
		t.Errorf("count after release %d, want %d", iso.CallbackCount(), before)
	}
}

// The row-12 pair: a getter answering an int from the receiver's field, and a
// one-string-argument method answering a string — each classic and fast.
func benchCallback(b *testing.B, src string, classic v8.FunctionCallback, spec string, fast v8.FastCallback) {
	for _, mode := range []string{"classic", "fast"} {
		b.Run(mode, func(b *testing.B) {
			iso := v8.NewIsolate()
			defer iso.Dispose()
			wrapper := v8.NewObjectTemplate(iso)
			wrapper.SetInternalFieldCount(1)
			if mode == "classic" {
				_ = wrapper.Set("m", v8.NewFunctionTemplate(iso, classic))
			} else {
				_ = wrapper.Set("m", v8.NewFastFunctionTemplate(iso, spec, fast))
			}
			ctx := v8.NewContext(iso)
			defer ctx.Close()
			obj, _ := wrapper.NewWrapper(ctx, 5, nil, nil, nil)
			g := ctx.Global()
			_ = g.Set("o", obj)
			fn := run(b, ctx, `(function (n) { let s = 0; for (let i = 0; i < n; i++) { s += `+src+`; } return s; })`)
			f, _ := fn.AsFunction()
			n, _ := v8.NewValue(iso, int32(1000))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out, err := f.Call(v8.Undefined(iso), n)
				if err != nil {
					b.Fatal(err)
				}
				out.Release()
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*1000), "ns/call")
		})
	}
}

func BenchmarkGetterFromField(b *testing.B) {
	benchCallback(b, `o.m()`,
		func(info *v8.FunctionCallbackInfo) *v8.Value {
			id, _ := info.ThisInternalField()
			v, _ := v8.NewValue(info.Context().Isolate(), int32(id))
			info.This().Release()
			return v
		}, "",
		func(info *v8.FastCallbackInfo) {
			id, _ := info.ThisInternalField()
			info.ReturnInt32(int32(id))
		})
}

func BenchmarkStringArgMethod(b *testing.B) {
	attrs := map[string]string{"data-x": "value"}
	benchCallback(b, `o.m('data-x').length`,
		func(info *v8.FunctionCallbackInfo) *v8.Value {
			arg := info.Args()[0]
			s := attrs[arg.String()]
			arg.Release()
			info.This().Release()
			v, _ := v8.NewValue(info.Context().Isolate(), s)
			return v
		}, "s",
		func(info *v8.FastCallbackInfo) {
			info.ReturnString(attrs[info.String(0)])
		})
}
