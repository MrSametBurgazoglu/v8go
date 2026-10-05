package v8go_test

import (
	"strings"
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// The value a failed RunScript threw is the object itself, not its text: a
// parse error is a SyntaxError of the context's realm, a throw of a primitive
// is that primitive, and a second take answers nil.
func TestTakeExceptionIsTheThrownValue(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	if _, err := ctx.RunScript(`let a = ;`, "parse.js"); err == nil {
		t.Fatal("a parse error ran")
	}
	thrown := ctx.TakeException()
	if thrown == nil {
		t.Fatal("no exception after a parse error")
	}
	global := ctx.Global()
	if err := global.Set("thrown", thrown); err != nil {
		t.Fatal(err)
	}
	isSyntax, err := ctx.RunScript(`thrown instanceof SyntaxError`, "check.js")
	if err != nil || !isSyntax.Boolean() {
		t.Errorf("the parse error is not this realm's SyntaxError (%v)", err)
	}
	if again := ctx.TakeException(); again != nil {
		t.Errorf("a second take answered %s", again.String())
	}

	if _, err := ctx.RunScript(`throw 7`, "throw.js"); err == nil {
		t.Fatal("throw 7 did not fail")
	}
	if seven := ctx.TakeException(); seven == nil || seven.Int32() != 7 {
		t.Errorf("throw 7 took %v", seven)
	}
}

// A module that does not parse says so before anything is compiled for it.
func TestCheckModuleSyntax(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()

	if err := iso.CheckModuleSyntax(`import x from "./x.js"; export const y = await x;`, "ok.js"); err != nil {
		t.Errorf("a valid module: %v", err)
	}
	err := iso.CheckModuleSyntax(`function f() { import v from "./v.js"; }`, "bad.js")
	if err == nil || !strings.HasPrefix(err.Error(), "SyntaxError: ") {
		t.Errorf("an import inside a function: %v", err)
	}
}

// import() of a module with top-level await settles when the await does:
// with the namespace once it fulfils, with the reason when it rejects.
func TestDynamicImportWaitsForTopLevelAwait(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	ctx.RegisterModule("later.js", `export let x = 1; await null; x = 2;`)
	ctx.RegisterModule("fails.js", `await null; throw new RangeError("late");`)
	ctx.RegisterModule("dep.js", `import { x } from "later.js"; export const y = x;`)
	if _, err := ctx.RunScript(`
		globalThis.out = [];
		import('later.js').then(ns => out.push('later:' + ns.x), e => out.push('later!' + e));
		import('fails.js').then(() => out.push('fails:fulfilled'), e => out.push('fails!' + e.name));
		import('dep.js').then(ns => out.push('dep:' + ns.y), e => out.push('dep!' + e));
	`, "main.js"); err != nil {
		t.Fatal(err)
	}
	iso.PerformMicrotaskCheckpoint()
	got, err := ctx.RunScript(`out.sort().join('|')`, "read.js")
	if err != nil {
		t.Fatal(err)
	}
	if want := "dep:2|fails!RangeError|later:2"; got.String() != want {
		t.Errorf("import() results\n got %s\nwant %s", got.String(), want)
	}
}

// A script's text is passed by length: U+0000 inside it is a character, not
// the end of the source.
func TestSourceKeepsNUL(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	got, err := ctx.RunScript("'a\x00b'.length", "nul.js")
	if err != nil || got.Int32() != 3 {
		t.Errorf("RunScript: %v %v", got, err)
	}
	script, err := iso.CompileUnboundScript("'a\x00bc'.length", "nul2.js", v8.CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := script.Run(ctx); err != nil || got.Int32() != 4 {
		t.Errorf("CompileUnboundScript: %v %v", got, err)
	}
	ctx.RegisterModule("nul.mjs", "export const n = 'a\x00bcd'.length;")
	if _, err := ctx.RunScript(`import('nul.mjs').then(ns => globalThis.n = ns.n)`, "m.js"); err != nil {
		t.Fatal(err)
	}
	iso.PerformMicrotaskCheckpoint()
	if got, err := ctx.RunScript(`globalThis.n`, "r.js"); err != nil || got.Int32() != 5 {
		t.Errorf("RegisterModule: %v %v", got, err)
	}
}
