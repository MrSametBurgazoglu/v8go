package v8go_test

import (
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// The import.meta initializer completes each module's import.meta after url:
// it is called once per module, with that module's object and URL, and what
// it adds is the module's own.
func TestImportMetaInitializer(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	init, err := ctx.RunScript(`(function (meta, url) {
		globalThis.calls = (globalThis.calls || 0) + 1;
		meta.resolve = ({ resolve(specifier) { return url + '>' + specifier; } }).resolve;
	})`, "init.js")
	if err != nil {
		t.Fatal(err)
	}
	fn, err := init.AsFunction()
	if err != nil {
		t.Fatal(err)
	}
	ctx.SetImportMetaInitializer(fn)
	ctx.RegisterModule("a.js", `globalThis.a = import.meta.resolve('x') + '|' + import.meta.url + '|' +
		Object.keys(import.meta).join(','); globalThis.a2 = import.meta.resolve('y');`)
	ctx.RegisterModule("b.js", `const { resolve } = import.meta; globalThis.b = resolve('z');`)
	if _, err := ctx.RunScript(`import('a.js').then(() => import('b.js'))`, "main.js"); err != nil {
		t.Fatal(err)
	}
	iso.PerformMicrotaskCheckpoint()
	got, err := ctx.RunScript(`[a, a2, b, calls].join(' ')`, "read.js")
	if err != nil {
		t.Fatal(err)
	}
	if want := "a.js>x|a.js|url,resolve a.js>y b.js>z 2"; got.String() != want {
		t.Errorf("got %q, want %q", got.String(), want)
	}

	// Removed: import.meta is url alone again, and an initializer that throws
	// does not reach the module.
	ctx.SetImportMetaInitializer(nil)
	ctx.RegisterModule("c.js", `globalThis.c = Object.keys(import.meta).join(',');`)
	thrower, _ := ctx.RunScript(`(function () { throw new Error('no'); })`, "throw.js")
	tfn, _ := thrower.AsFunction()
	if _, err := ctx.RunScript(`import('c.js')`, "main2.js"); err != nil {
		t.Fatal(err)
	}
	iso.PerformMicrotaskCheckpoint()
	ctx.SetImportMetaInitializer(tfn)
	ctx.RegisterModule("d.js", `globalThis.d = typeof import.meta.url;`)
	if _, err := ctx.RunScript(`import('d.js')`, "main3.js"); err != nil {
		t.Fatal(err)
	}
	iso.PerformMicrotaskCheckpoint()
	if got, _ := ctx.RunScript(`c + ' ' + d`, "read2.js"); got.String() != "url string" {
		t.Errorf("after removal and with a throwing initializer: %q, want %q", got.String(), "url string")
	}
}
