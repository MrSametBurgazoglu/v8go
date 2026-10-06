package v8go_test

import (
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// A string with unpaired surrogates survives StringWTF8 and NewStringWTF8
// unchanged, where String replaces each with U+FFFD; a well-formed string
// reads the same either way.
func TestStringWTF8RoundTrip(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	val, err := ctx.RunScript(`'a\uD800b\uDFFFc😀'`, "wtf8.js")
	if err != nil {
		t.Fatal(err)
	}
	if got := val.String(); got != "a�b�c\U0001F600" {
		t.Errorf("String() = %q", got)
	}
	wtf8 := val.StringWTF8()
	if wtf8 != "a\xED\xA0\x80b\xED\xBF\xBFc\U0001F600" {
		t.Fatalf("StringWTF8() = %q", wtf8)
	}
	back, err := v8.NewStringWTF8(iso, wtf8)
	if err != nil {
		t.Fatal(err)
	}
	global := ctx.Global()
	if err := global.Set("back", back); err != nil {
		t.Fatal(err)
	}
	same, err := ctx.RunScript(`back === 'a\uD800b\uDFFFc😀' && back.length === 7`, "check.js")
	if err != nil {
		t.Fatal(err)
	}
	if !same.Boolean() {
		t.Error("NewStringWTF8 did not give back the same string")
	}
	plain, _ := ctx.RunScript(`'plain ü'`, "plain.js")
	if plain.StringWTF8() != plain.String() {
		t.Error("a well-formed string read differently")
	}
	empty, err := v8.NewStringWTF8(iso, "")
	if err != nil || empty.String() != "" || !empty.IsString() {
		t.Errorf("empty string: %v %v", empty, err)
	}
}
