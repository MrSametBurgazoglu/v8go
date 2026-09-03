package v8go_test

import (
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

func TestValueIdentityHash(t *testing.T) {
	ctx := v8.NewContext()
	defer ctx.Isolate().Dispose()
	defer ctx.Close()
	a, _ := ctx.RunScript("globalThis.f = function(){}; f", "a.js")
	b, _ := ctx.RunScript("f", "b.js")
	c, _ := ctx.RunScript("(function(){})", "c.js")
	n, _ := ctx.RunScript("42", "n.js")
	if a.IdentityHash() == 0 || a.IdentityHash() != b.IdentityHash() {
		t.Fatalf("same object, hashes %d and %d", a.IdentityHash(), b.IdentityHash())
	}
	if c.IdentityHash() == a.IdentityHash() {
		t.Fatalf("two different functions share hash %d (possible but improbable)", a.IdentityHash())
	}
	if n.IdentityHash() != 0 {
		t.Fatalf("a number answered hash %d", n.IdentityHash())
	}
}
