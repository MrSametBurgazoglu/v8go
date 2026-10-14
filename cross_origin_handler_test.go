package v8go_test

import (
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// A global proxy from a context with another security token answers through
// the accessing context's hook: reads, writes, `in` and own keys go to it,
// what it throws is thrown, and a context without one is refused as before.
// Two contexts sharing a token never reach the hook.
func TestCrossOriginHandlerAnswersThroughTheAccessorsHook(t *testing.T) {
	t.Parallel()

	iso := v8.NewIsolate()
	defer iso.Dispose()

	global := v8.NewObjectTemplate(iso)
	global.SetCrossOriginHandler()
	target := v8.NewContext(iso, global)
	defer target.Close()
	accessor := v8.NewContext(iso)
	defer accessor.Close()
	plain := v8.NewContext(iso)
	defer plain.Close()

	_, err := target.RunScript(`var secret = 'mine'; var postMessage = 'real';`, "target.js")
	fatalIf(t, err)
	fatalIf(t, accessor.Global().Set("w", target.Global()))
	fatalIf(t, plain.Global().Set("w", target.Global()))

	hook, err := accessor.RunScript(`(function (receiver, key, op, value) {
		log.push([receiver === w, String(key), op].join(':'));
		if (op === 0) {
			if (key === 'postMessage') { return 'allowed'; }
			if (typeof key === 'number') { return undefined; }
			throw new Error('refused ' + String(key));
		}
		if (op === 1) { written = value; return; }
		if (op === 2) { return key === 'postMessage'; }
		if (op === 4) { return []; }
		throw new Error('refused');
	})`, "hook.js")
	fatalIf(t, err)
	_, err = accessor.RunScript(`var log = []; var written;`, "log.js")
	fatalIf(t, err)
	accessor.SetCrossOriginHook(hook)

	got, err := accessor.RunScript(`[
		w.postMessage,
		(function () { try { return w.secret; } catch (e) { return e.message; } })(),
		(w.location = 'x', written),
		'postMessage' in w,
		Object.keys(w).length,
		w[0] === undefined ? 'idx' : 'no',
	].join(',') + ' ' + log.join(' ')`, "use.js")
	if err != nil {
		t.Fatalf("access through the hook: %v", err)
	}
	want := "allowed,refused secret,x,true,0,idx true:postMessage:0 true:secret:0 true:location:1 true:postMessage:2 true:undefined:4 true:0:0"
	if got.String() != want {
		t.Errorf("got  %q\nwant %q", got.String(), want)
	}

	// No hook: V8's own refusal.
	if _, err := plain.RunScript(`w.postMessage`, "plain.js"); err == nil {
		t.Errorf("a context with no hook read a cross-origin global")
	}

	// One token: the ordinary access, the hook never consulted.
	token := target.SecurityToken()
	accessor.SetSecurityToken(token)
	got, err = accessor.RunScript(`log = []; [w.secret, w.postMessage, log.length].join(',')`, "same.js")
	fatalIf(t, err)
	if got.String() != "mine,real,0" {
		t.Errorf("same token: got %q, want %q", got.String(), "mine,real,0")
	}
}

// The rest of HTML's cross-origin operations go to the hook too:
// [[GetOwnProperty]] answers with the hook's descriptor, own keys may carry
// symbols, an attribute answer from `has` decides enumerability, and what V8
// has no interceptor for -- [[SetPrototypeOf]], a definition -- is the
// hook's to throw. An ordinary object from such a template is refused in
// every context, its own included.
func TestCrossOriginHandlerAnswersDescriptorsKeysAndRefusals(t *testing.T) {
	t.Parallel()

	iso := v8.NewIsolate()
	defer iso.Dispose()

	tmpl := v8.NewObjectTemplate(iso)
	tmpl.SetCrossOriginHandler()
	tmpl.SetInternalFieldCount(1)
	ctx := v8.NewContext(iso)
	defer ctx.Close()
	obj, err := tmpl.NewInstance(ctx)
	fatalIf(t, err)
	fatalIf(t, obj.SetInternalField(0, int32(7)))
	fatalIf(t, ctx.Global().Set("x", obj))

	hook, err := ctx.RunScript(`(function (receiver, key, op, value) {
		key = typeof key === 'number' ? String(key) : key;
		var desc = { value: 'v', writable: false, enumerable: key === '0', configurable: true };
		switch (op) {
		case 0: return 'got ' + String(key);
		case 2: return key === '0' ? 0 : true;
		case 4: return ['0', 'a', 'then', Symbol.toStringTag];
		case 5: if (key === 'a' || key === '0') { return desc; } throw new Error('nodesc ' + String(key));
		case 3: case 6: case 7: throw new Error('refused ' + op + ':' + value);
		}
	})`, "hook.js")
	fatalIf(t, err)
	ctx.SetCrossOriginHook(hook)

	got, err := ctx.RunScript(`(function () {
		function t(f) { try { return String(f()); } catch (e) { return 'threw ' + e.message; } }
		return [
			t(function () { return x.a; }),
			t(function () { var d = Object.getOwnPropertyDescriptor(x, 'a'); return [d.value, d.writable, d.enumerable, d.configurable].join('/'); }),
			t(function () { return Object.getOwnPropertyDescriptor(x, 0).enumerable; }),
			t(function () { return Object.getOwnPropertyDescriptor(x, 'b'); }),
			t(function () { return Object.keys(x).join('+'); }),
			t(function () { return Reflect.ownKeys(x).length; }),
			t(function () { return Object.getOwnPropertySymbols(x).length; }),
			t(function () { return delete x.a; }),
			t(function () { Object.defineProperty(x, 'a', { value: 1 }); return 'defined'; }),
			t(function () { return Object.setPrototypeOf(x, {}); }),
			t(function () { return Object.getPrototypeOf(x); }),
		].join(' | ');
	})()`, "use.js")
	fatalIf(t, err)
	want := "got a | v/false/false/true | true | threw nodesc b | 0 | 4 | 1 | threw refused 7:2 | threw refused 7:2 | threw refused 7:2 | null"
	if got.String() != want {
		t.Errorf("got  %q\nwant %q", got.String(), want)
	}
	if v := obj.GetInternalField(0); v == nil || v.Int32() != 7 {
		t.Errorf("internal field: %v", v)
	}
}
