package v8go_test

import (
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// A module compiled in one context is rebuilt in another over the same code:
// an object of the second realm, which instantiates there.
func TestWasmModuleObjectFromCompiled(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	src := v8.NewContext(iso)
	defer src.Close()
	dst := v8.NewContext(iso)
	defer dst.Close()

	// (module (func (export "answer") (result i32) i32.const 42))
	mod, err := src.RunScript(`new WebAssembly.Module(new Uint8Array([
		0,97,115,109,1,0,0,0,1,5,1,96,0,1,127,3,2,1,0,7,10,1,6,97,110,115,119,
		101,114,0,0,10,6,1,4,0,65,42,11]))`, "src.js")
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := mod.WasmCompiledModule()
	if err != nil {
		t.Fatal(err)
	}
	moved, err := v8.NewWasmModuleObjectFromCompiled(dst, compiled)
	compiled.Release()
	if err != nil {
		t.Fatal(err)
	}
	if err := dst.Global().Set("moved", moved); err != nil {
		t.Fatal(err)
	}
	got, err := dst.RunScript(`(moved instanceof WebAssembly.Module) + ":" +
		new WebAssembly.Instance(moved).exports.answer()`, "dst.js")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "true:42" {
		t.Errorf("moved module = %q, want true:42", got.String())
	}

	notModule, _ := src.RunScript(`({})`, "plain.js")
	if _, err := notModule.WasmCompiledModule(); err == nil {
		t.Error("a plain object answered a compiled module")
	}
}
