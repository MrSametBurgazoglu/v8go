package v8go_test

import (
	"sort"
	"strconv"
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// storageLike builds an object shaped like WebIDL's Storage: a named getter,
// setter, deleter, descriptor and enumerator over a Go map, plus a definer that
// refuses accessor definitions. Every operation is observable from script.
func storageLike(t *testing.T, iso *v8.Isolate, store map[string]string) *v8.ObjectTemplate {
	t.Helper()
	str := func(s string) *v8.Value { v, _ := v8.NewValue(iso, s); return v }
	boolean := func(b bool) *v8.Value { v, _ := v8.NewValue(iso, b); return v }
	tmpl := v8.NewObjectTemplate(iso)
	tmpl.SetNamedPropertyInterceptor(v8.PropertyInterceptor{
		Getter: func(info *v8.FunctionCallbackInfo) *v8.Value {
			if v, ok := store[info.Args()[0].String()]; ok {
				return str(v)
			}
			return nil
		},
		Setter: func(info *v8.FunctionCallbackInfo) *v8.Value {
			args := info.Args()
			if args[0].String() == "readonly" {
				return boolean(false)
			}
			store[args[0].String()] = args[1].String()
			return boolean(true)
		},
		Deleter: func(info *v8.FunctionCallbackInfo) *v8.Value {
			name := info.Args()[0].String()
			if _, ok := store[name]; !ok {
				return nil
			}
			delete(store, name)
			return boolean(true)
		},
		Enumerator: func(info *v8.FunctionCallbackInfo) *v8.Value {
			if len(info.Args()) != 0 {
				t.Errorf("enumerator called with %d arguments, want 0", len(info.Args()))
			}
			names := make([]string, 0, len(store))
			for k := range store {
				names = append(names, k)
			}
			sort.Strings(names)
			vals := make([]*v8.Value, len(names))
			for i, n := range names {
				vals[i] = str(n)
			}
			arr, _ := v8.NewArrayOf(info.Context(), vals)
			return arr
		},
		Definer: func(info *v8.FunctionCallbackInfo) *v8.Value {
			desc := info.Args()[1].Object()
			if has, _ := desc.Get("get"); has != nil && !has.IsUndefined() {
				return boolean(false)
			}
			value, _ := desc.Get("value")
			store[info.Args()[0].String()] = value.String()
			return boolean(true)
		},
		Descriptor: func(info *v8.FunctionCallbackInfo) *v8.Value {
			v, ok := store[info.Args()[0].String()]
			if !ok {
				return nil
			}
			ctx := info.Context()
			out, err := ctx.RunScript(`({value: `+strconv.Quote(v)+`, writable: true, enumerable: true, configurable: true})`, "desc.js")
			if err != nil {
				t.Error(err)
				return nil
			}
			return out
		},
	})
	return tmpl
}

func TestPropertyInterceptorAllNamedOperations(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	store := map[string]string{"a": "1"}
	global := v8.NewObjectTemplate(iso)
	if err := global.Set("s", storageLike(t, iso, store)); err != nil {
		t.Fatal(err)
	}
	ctx := v8.NewContext(iso, global)
	defer ctx.Close()

	for _, c := range []struct{ expr, want string }{
		{`s.a`, "1"},
		{`s.b = 'two'; s.b`, "two"},
		{`JSON.stringify(Object.keys(s))`, `["a","b"]`},
		{`JSON.stringify(Object.getOwnPropertyDescriptor(s, 'a'))`, `{"value":"1","writable":true,"enumerable":true,"configurable":true}`},
		{`'b' in s`, "true"},
		{`delete s.b; 'b' in s`, "false"},
		{`s.readonly = 1; 'readonly' in s`, "false"},
		{`(() => { 'use strict'; try { s.readonly = 1; return 'no throw'; } catch (e) { return e.constructor.name; } })()`, "TypeError"},
		{`Reflect.defineProperty(s, 'c', {get() {}})`, "false"},
		{`Object.defineProperty(s, 'c', {value: 'three'}); s.c`, "three"},
		{`typeof s.hasOwnProperty`, "function"},
	} {
		got, err := ctx.RunScript(c.expr, "t.js")
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got.String() != c.want {
			t.Errorf("%s = %q, want %q", c.expr, got.String(), c.want)
		}
	}
	if store["b"] != "" || store["c"] != "three" {
		t.Errorf("store = %v", store)
	}
}

// An indexed interceptor with a descriptor and a setter: what a <select>'s
// options and a form's controls need for Object.getOwnPropertyDescriptor and
// select[i] = option.
func TestPropertyInterceptorIndexedOperations(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	items := []string{"x", "y"}
	tmpl := v8.NewObjectTemplate(iso)
	tmpl.SetIndexedPropertyInterceptor(v8.PropertyInterceptor{
		Getter: func(info *v8.FunctionCallbackInfo) *v8.Value {
			i := int(info.Args()[0].Number())
			if i >= len(items) {
				return nil
			}
			v, _ := v8.NewValue(iso, items[i])
			return v
		},
		Setter: func(info *v8.FunctionCallbackInfo) *v8.Value {
			i := int(info.Args()[0].Number())
			for len(items) <= i {
				items = append(items, "")
			}
			items[i] = info.Args()[1].String()
			v, _ := v8.NewValue(iso, true)
			return v
		},
		Descriptor: func(info *v8.FunctionCallbackInfo) *v8.Value {
			i := int(info.Args()[0].Number())
			if i >= len(items) {
				return nil
			}
			out, _ := info.Context().RunScript(`({value: `+strconv.Quote(items[i])+`, writable: true, enumerable: true, configurable: true})`, "d.js")
			return out
		},
		Enumerator: func(info *v8.FunctionCallbackInfo) *v8.Value {
			vals := make([]*v8.Value, len(items))
			for i := range items {
				vals[i], _ = v8.NewValue(iso, int32(i))
			}
			arr, _ := v8.NewArrayOf(info.Context(), vals)
			return arr
		},
	})
	global := v8.NewObjectTemplate(iso)
	global.Set("c", tmpl)
	ctx := v8.NewContext(iso, global)
	defer ctx.Close()
	for _, c := range []struct{ expr, want string }{
		{`c[1]`, "y"},
		{`c[3] = 'w'; c[3]`, "w"},
		{`Object.getOwnPropertyDescriptor(c, 0).value`, "x"},
		{`Object.getOwnPropertyDescriptor(c, 9) === undefined`, "true"},
		{`JSON.stringify(Object.keys(c))`, `["0","1","2","3"]`},
	} {
		got, err := ctx.RunScript(c.expr, "t.js")
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got.String() != c.want {
			t.Errorf("%s = %q, want %q", c.expr, got.String(), c.want)
		}
	}
}

// A throwing callback makes the operation throw rather than be ignored.
func TestPropertyInterceptorThrows(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	tmpl := v8.NewObjectTemplate(iso)
	tmpl.SetNamedPropertyInterceptor(v8.PropertyInterceptor{
		Setter: func(info *v8.FunctionCallbackInfo) *v8.Value {
			msg, _ := v8.NewValue(iso, "nope")
			return iso.ThrowException(msg)
		},
		Flags: v8.HandlerOnlyInterceptStrings,
	})
	global := v8.NewObjectTemplate(iso)
	global.Set("o", tmpl)
	ctx := v8.NewContext(iso, global)
	defer ctx.Close()
	got, err := ctx.RunScript(`try { o.x = 1; 'no throw' } catch (e) { String(e) }`, "t.js")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "nope" {
		t.Errorf("got %q, want the setter's exception", got.String())
	}
}

// SetImmutableProto: the prototype given at creation stays, as WebIDL asks of
// a [Global] object and of the window's named properties object.
func TestObjectTemplateSetImmutableProto(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	fn := v8.NewFunctionTemplate(iso, func(info *v8.FunctionCallbackInfo) *v8.Value { return nil })
	fn.InstanceTemplate().SetImmutableProto()
	ctx := v8.NewContext(iso)
	defer ctx.Close()
	ctor := fn.GetFunction(ctx)
	proto, _ := ctx.RunScript(`({marker: 'kept'})`, "p.js")
	if err := ctor.Object().Set("prototype", proto); err != nil {
		t.Fatal(err)
	}
	obj, err := ctor.NewInstance()
	if err != nil {
		t.Fatal(err)
	}
	ctx.Global().Set("o", obj)
	for _, c := range []struct{ expr, want string }{
		{`o.marker`, "kept"},
		{`Object.setPrototypeOf(o, Object.getPrototypeOf(o)) === o`, "true"},
		{`Reflect.setPrototypeOf(o, {})`, "false"},
		{`try { Object.setPrototypeOf(o, null); 'no throw' } catch (e) { e.constructor.name }`, "TypeError"},
		{`try { o.__proto__ = {}; 'no throw' } catch (e) { e.constructor.name }`, "TypeError"},
		{`o.marker`, "kept"},
	} {
		got, err := ctx.RunScript(c.expr, "t.js")
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got.String() != c.want {
			t.Errorf("%s = %q, want %q", c.expr, got.String(), c.want)
		}
	}
}
