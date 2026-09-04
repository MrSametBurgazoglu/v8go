package v8go_test

import (
	"strings"
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// V8's default ArrayBuffer allocator calls FatalProcessOutOfMemory when the
// underlying allocation fails, which is an abort: no exception, no Go panic,
// and the whole embedding process goes. For an embedder running untrusted
// script — a browser — that turns four lines into a way to end the program:
//
//	new Int32Array(536870911)
//
// The allocator installed here refuses past a ceiling and returns nullptr,
// which is what the ArrayBuffer::Allocator contract asks for on failure and
// which V8 turns into the RangeError every engine throws.
//
// The test reaching its own assertions is half of what it proves: a process
// that aborted would not get here.
func TestHugeArrayBufferThrowsRatherThanAborting(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	// Two gibibytes, past the ceiling.
	if _, err := ctx.RunScript("new Int32Array(536870911)", "huge.js"); err == nil {
		t.Fatal("a two-gibibyte typed array was allocated; the ceiling did not apply")
	} else if !strings.Contains(err.Error(), "RangeError") {
		t.Errorf("the failure is %q, want a RangeError", err.Error())
	}

	// And the isolate is still usable afterwards, which is the other half of
	// the point: a refused allocation is an exception, not a broken engine.
	value, err := ctx.RunScript("1 + 1", "after.js")
	if err != nil {
		t.Fatalf("the isolate did not survive the refusal: %v", err)
	}
	if value.String() != "2" {
		t.Errorf("after the refusal, 1 + 1 = %s", value.String())
	}

	// An ordinary buffer still allocates: the ceiling is a ceiling, not a ban.
	if _, err := ctx.RunScript("new Uint8Array(1024 * 1024).length", "small.js"); err != nil {
		t.Errorf("a one-megabyte array was refused: %v", err)
	}
}
