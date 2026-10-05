package v8go_test

import (
	"strings"
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// A function an extension defines prints as native code; one a script defines
// prints its source.
func TestExtensionFunctionsAreNative(t *testing.T) {
	if err := v8.RegisterExtension("test/native-registry", `
		(function () {
			const registry = {};
			registry.plain = function plain(a, b) { return a + b; };
			registry.getter = Object.getOwnPropertyDescriptor({ get value() { return 1; } }, 'value').get;
			registry.arrow = () => 1;
			registry.method = ({ m() { return 2; } }).m;
			registry.make = function () { return function inner() { return 3; }; };
			globalThis.__nativeRegistry = registry;
		})();
	`); err != nil {
		t.Fatal(err)
	}
	iso := v8.NewIsolate()
	defer iso.Dispose()
	iso.SetContextExtensions([]string{"test/native-registry"})
	ctx := v8.NewContext(iso)
	defer ctx.Close()
	for expr, want := range map[string]string{
		`Function.prototype.toString.call(__nativeRegistry.plain)`:  "function plain() { [native code] }",
		`Function.prototype.toString.call(__nativeRegistry.getter)`: "function get value() { [native code] }",
		`Function.prototype.toString.call(__nativeRegistry.arrow)`:  "function () { [native code] }",
		`Function.prototype.toString.call(__nativeRegistry.method)`: "function m() { [native code] }",
		`Function.prototype.toString.call(__nativeRegistry.make())`: "function inner() { [native code] }",
		`__nativeRegistry.plain(2, 3)`:                              "5",
		`String(function userFn() { return 1; })`:                   "function userFn() { return 1; }",
	} {
		got, err := ctx.RunScript(expr, "probe.js")
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if got.String() != want {
			t.Errorf("%s = %q, want %q", expr, got.String(), want)
		}
	}
	// A context of an isolate without the configuration does not install it.
	other := v8.NewContext()
	defer other.Close()
	if got, _ := other.RunScript(`typeof __nativeRegistry`, "probe.js"); got.String() != "undefined" {
		t.Errorf("an unconfigured context installed the extension: typeof = %s", got.String())
	}
	// And a stack through extension code: what does a page see?
	got, err := ctx.RunScript(`(function () { try { __nativeRegistry.plain.call(null, Symbol(), 1); } catch (e) { return e.stack; } })()`, "probe.js")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("stack through extension code:\n%s", got.String())
	if !strings.Contains(got.String(), "TypeError") {
		t.Errorf("stack = %q", got.String())
	}
}

func TestExtensionSourceMustBeLatin1(t *testing.T) {
	if err := v8.RegisterExtension("test/not-latin1", "var x = '—';"); err == nil {
		t.Error("a source with U+2014 registered")
	}
	if err := v8.RegisterExtension("test/latin1", "var y = 'é';"); err != nil {
		t.Errorf("a Latin-1 source was refused: %v", err)
	}
}
