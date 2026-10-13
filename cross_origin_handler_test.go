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
