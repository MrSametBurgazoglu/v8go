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
