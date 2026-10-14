package v8go_test

import (
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// A function made in one context names that context, read from another;
// a primitive names none.
func TestValueCreationContextRef(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	a := v8.NewContext(iso)
	defer a.Close()
	b := v8.NewContext(iso)
	defer b.Close()
	fn, err := a.RunScript("(function () {})", "a.js")
	if err != nil {
		t.Fatal(err)
	}
	if got := fn.CreationContextRef(); got != a.Ref() || got == b.Ref() {
		t.Fatalf("creation context ref %d, want a's %d (b is %d)", got, a.Ref(), b.Ref())
	}
	n, _ := v8.NewValue(iso, int32(1))
	if got := n.CreationContextRef(); got != 0 {
		t.Fatalf("a number's creation context ref %d, want 0", got)
	}
}
