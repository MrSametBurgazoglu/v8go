package v8go_test

import (
	"errors"
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// import.meta.resolve answers through the embedder, with the module's URL as
// the base, and throws a TypeError when the embedder refuses.
func TestImportMetaResolve(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	ctx.RegisterModule("https://a.test/m.js", `
		globalThis.out = [typeof import.meta.resolve, import.meta.resolve.length,
			import.meta.resolve.name, import.meta.resolve('x'),
			import.meta.resolve({ toString() { return 'y'; } })];
		try { import.meta.resolve('bad'); } catch (e) { out.push(e.constructor.name + ':' + e.message); }
		try { new import.meta.resolve('x'); } catch (e) { out.push(e.constructor.name); }
	`)
	ctx.SetImportMetaResolver(func(specifier, moduleURL string) (string, error) {
		if specifier == "bad" {
			return "", errors.New("no such module")
		}
		return moduleURL + "?" + specifier, nil
	})
	if _, err := ctx.RunScript(`import('https://a.test/m.js').catch(e => { globalThis.out = [String(e)]; })`, "main.js"); err != nil {
		t.Fatal(err)
	}
	iso.PerformMicrotaskCheckpoint()
	got, err := ctx.RunScript(`out.join('|')`, "read.js")
	if err != nil {
		t.Fatal(err)
	}
	want := "function|1|resolve|https://a.test/m.js?x|https://a.test/m.js?y|TypeError:no such module|TypeError"
	if got.String() != want {
		t.Errorf("got  %s\nwant %s", got.String(), want)
	}
}

// Without a resolver, import.meta has no resolve: absent, not a function
// that always fails.
func TestImportMetaResolveAbsentWithoutResolver(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	ctx.RegisterModule("m.js", `globalThis.out = 'resolve' in import.meta;`)
	if _, err := ctx.RunScript(`import('m.js')`, "main.js"); err != nil {
		t.Fatal(err)
	}
	iso.PerformMicrotaskCheckpoint()
	got, _ := ctx.RunScript(`String(out)`, "read.js")
	if got.String() != "false" {
		t.Errorf("'resolve' in import.meta = %s without a resolver", got.String())
	}
}
