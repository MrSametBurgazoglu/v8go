package v8go_test

import (
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// A private-keyed property is the embedder's alone: script sees no key for
// it, by any reflection, and the embedder reads back what it set.
func TestObjectPrivateIsInvisibleToScript(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()
	global := ctx.Global()
	if err := global.SetPrivate("embedder.slot", "secret"); err != nil {
		t.Fatal(err)
	}
	got, err := global.GetPrivate("embedder.slot")
	if err != nil || got.String() != "secret" {
		t.Fatalf("GetPrivate = %v, %v", got, err)
	}
	seen, err := ctx.RunScript(`[Reflect.ownKeys(globalThis).length, Object.getOwnPropertySymbols(globalThis).length,
		JSON.stringify(Object.getOwnPropertyNames(globalThis).filter(n => n.indexOf('embedder') >= 0)),
		String(globalThis[Symbol.for('embedder.slot')])].join('|')`, "probe.js")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := ctx.RunScript(`Object.getOwnPropertySymbols(globalThis).length`, "count.js")
	if want := "|" + before.String() + "|[]|undefined"; len(seen.String()) == 0 || seen.String()[len(seen.String())-len(want):] != want {
		t.Errorf("script saw %q", seen.String())
	}
	global.DeletePrivate("embedder.slot")
	if got, _ := global.GetPrivate("embedder.slot"); !got.IsUndefined() {
		t.Errorf("after DeletePrivate the slot reads %v", got)
	}
	missing, _ := global.GetPrivate("never.set")
	if !missing.IsUndefined() {
		t.Errorf("an unset private reads %v", missing)
	}
}

// The same private as a value, for the embedder's own script: every way
// script uses a property key works with it, and no reflection lists it.
func TestPrivateSymbolAsScriptKey(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()
	mint, err := ctx.PrivateSymbolFunction()
	if err != nil {
		t.Fatal(err)
	}
	key := ctx.PrivateSymbol("embedder.slot")
	// Value.IsSymbol answers for PUBLIC symbols only; a private is a Name
	// V8 keeps apart.
	if key.IsSymbol() || key.IsUndefined() {
		t.Fatalf("PrivateSymbol: IsSymbol %v, IsUndefined %v", key.IsSymbol(), key.IsUndefined())
	}
	probe, err := ctx.RunScript(`(function (mint, goKey) {
	  const k = mint('embedder.slot');
	  const out = [];
	  out.push(k === goKey, k === mint('embedder.slot'), k !== mint('embedder.other'), typeof k);
	  const o = {};
	  o[k] = 1;
	  out.push(o[k], k in o, Object.getOwnPropertySymbols(o).length, Reflect.ownKeys(o).length,
	    JSON.stringify(Object.getOwnPropertyDescriptors(o)), Object.assign({}, o)[k]);
	  Object.defineProperty(o, k, { get() { return 'got'; }, configurable: true });
	  out.push(o[k]);
	  delete o[k];
	  out.push(k in o);
	  const f = Object.freeze({});
	  f[k] = 2;
	  out.push(f[k]);
	  const inherited = Object.create((function () { const p = {}; p[k] = 3; return p; })());
	  out.push(inherited[k]);
	  let trapped = 0;
	  const p = new Proxy({}, { get() { trapped++; }, set() { trapped++; return true; }, has() { trapped++; return true; },
	    ownKeys() { trapped++; return []; }, defineProperty() { trapped++; return true; } });
	  p[k] = 4;
	  out.push(p[k], k in p, trapped);
	  globalThis[k] = 5;
	  out.push(globalThis[k], Object.getOwnPropertySymbols(globalThis).indexOf(k), Symbol.keyFor(k), Symbol.for('embedder.slot') === k);
	  const fresh = mint('embedder.slot', true), other = mint('embedder.slot', true);
	  out.push(fresh !== other, fresh !== k);
	  const f2 = {}; f2[fresh] = 6;
	  out.push(f2[fresh], f2[other], Reflect.ownKeys(f2).length);
	  return out.join(',');
	})`, "private.js")
	if err != nil {
		t.Fatal(err)
	}
	fn, _ := probe.AsFunction()
	got, err := fn.Call(v8.Undefined(iso), mint, key)
	if err != nil {
		t.Fatal(err)
	}
	// On a Proxy a private-keyed write stores nothing and reaches no trap:
	// the embedder keys its slots on its own objects, never on a page's proxy.
	const want = "true,true,true,symbol,1,true,0,0,{},,got,false,2,,,false,0,5,-1,,false,true,true,6,,0"
	if got.String() != want {
		t.Errorf("script with a private key:\n got  %s\n want %s", got.String(), want)
	}
	// Go's name-keyed accessors see the property script set.
	global := ctx.Global()
	if v, _ := global.GetPrivate("embedder.slot"); v.String() != "5" {
		t.Errorf("GetPrivate after script set = %v", v)
	}
	// And Go's key-valued accessors work with it.
	obj, _ := ctx.RunScript(`({})`, "o.js")
	o, _ := obj.AsObject()
	if err := o.SetKey(key, "via-key"); err != nil {
		t.Fatal(err)
	}
	if v, _ := o.GetPrivate("embedder.slot"); v.String() != "via-key" {
		t.Errorf("GetPrivate after SetKey = %v", v)
	}
	if v, _ := o.GetKey(key); v.String() != "via-key" {
		t.Errorf("GetKey = %v", v)
	}
	if n, _ := ctx.RunScript(`0`, "n.js"); n == nil {
		t.Fatal("context lost")
	}
}

// A template's private property lands on every instance, where a script
// holding the key reads it and no reflection lists it.
func TestTemplateSetPrivate(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	tmpl := v8.NewObjectTemplate(iso)
	if err := tmpl.SetPrivate("embedder.brand", true); err != nil {
		t.Fatal(err)
	}
	ctx := v8.NewContext(iso)
	defer ctx.Close()
	obj, err := tmpl.NewInstance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key := ctx.PrivateSymbol("embedder.brand")
	probe, _ := ctx.RunScript(`(function (o, k) { return [o[k], Reflect.ownKeys(o).length, ({})[k]].join(','); })`, "t.js")
	fn, _ := probe.AsFunction()
	got, err := fn.Call(v8.Undefined(iso), obj, key)
	if err != nil || got.String() != "true,0," {
		t.Errorf("template private = %v, %v", got, err)
	}
}
