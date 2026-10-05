package v8go_test

import (
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// import() hands its attributes to the embedder, evaluates the key the
// embedder answers with, and rejects with the error class it names.
func TestDynamicImportResolverSeesAttributes(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	var seen []v8.DynamicImport
	ctx.RegisterModule("data.json#json", `export default {"a": 1};`)
	ctx.RegisterModule("plain.js", `export const x = 42;`)
	ctx.SetDynamicImportResolver(func(req v8.DynamicImport) (string, *v8.DynamicImportFailure) {
		seen = append(seen, req)
		switch {
		case req.Specifier == "data.json" && req.Attributes["type"] == "json":
			return "data.json#json", nil
		case req.Specifier == "data.json":
			return "", &v8.DynamicImportFailure{Kind: v8.DynamicImportTypeError, Message: "not JavaScript"}
		case req.Attributes["type"] == "bogus":
			return "", &v8.DynamicImportFailure{Kind: v8.DynamicImportTypeError, Message: "unknown type"}
		case req.Specifier == "missing.js":
			return "missing.js", nil
		}
		return "", nil
	})

	if _, err := ctx.RunScript(`
		globalThis.out = [];
		const record = (p) => p.then(v => out.push('ok:' + JSON.stringify(v.default ?? v.x)),
		                             e => out.push(e.constructor.name + ':' + e.message));
		record(import('data.json', {with: {type: 'json'}}));
		record(import('data.json'));
		record(import('plain.js'));
		record(import('plain.js', {with: {type: 'bogus'}}));
		record(import('missing.js'));
	`, "main.js"); err != nil {
		t.Fatal(err)
	}
	iso.PerformMicrotaskCheckpoint()
	got, err := ctx.RunScript(`out.join('|')`, "read.js")
	if err != nil {
		t.Fatal(err)
	}
	want := `ok:{"a":1}|TypeError:not JavaScript|ok:42|TypeError:unknown type|` +
		`TypeError:Failed to fetch dynamically imported module: missing.js`
	if got.String() != want {
		t.Errorf("import() results\n got %s\nwant %s", got.String(), want)
	}
	if len(seen) != 5 || seen[0].Attributes["type"] != "json" || seen[0].Referrer != "main.js" ||
		len(seen[1].Attributes) != 0 {
		t.Errorf("resolver saw %+v", seen)
	}
}
