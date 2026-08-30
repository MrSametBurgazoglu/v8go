// Copyright 2019 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// The full debugger round trip: enable the domains, set a breakpoint by URL,
// run a script that hits it, observe the paused event with a call stack, and
// resume from inside the pause loop — which is the only place a resume can
// come from, since V8 holds the thread while paused.
func TestInspectorBreakpointPausesAndResumes(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	inspector := v8.NewInspector(iso)
	defer inspector.Dispose()
	inspector.ContextCreated(ctx)

	session := inspector.Connect()
	defer session.Dispose()

	var notifications []map[string]interface{}
	session.Message = func(callID int, message []byte) {
		if callID >= 0 {
			return // responses; the test asserts on events
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal(message, &parsed); err == nil {
			notifications = append(notifications, parsed)
		}
	}

	nextID := 0
	send := func(method, params string) {
		nextID++
		if params == "" {
			params = "{}"
		}
		session.DispatchProtocolMessage([]byte(fmt.Sprintf(
			`{"id":%d,"method":%q,"params":%s}`, nextID, method, params)))
	}

	send("Runtime.enable", "")
	send("Debugger.enable", "")
	send("Debugger.setBreakpointByUrl", `{"lineNumber":2,"url":"pause.js"}`)

	resumed := false
	inspector.PauseHandler = func() bool {
		// Paused at the breakpoint: resuming is a protocol message dispatched
		// from right here, on the thread V8 is holding.
		resumed = true
		send("Debugger.resume", "")
		return true
	}

	source := strings.Join([]string{
		"function target() {",
		"  let a = 1;",
		"  a = a + 41;", // line 2: the breakpoint
		"  return a;",
		"}",
		"target();",
	}, "\n")
	value, err := ctx.RunScript(source, "pause.js")
	if err != nil {
		t.Fatalf("the script failed: %v", err)
	}
	if got := value.Int32(); got != 42 {
		t.Errorf("the script answered %d after resuming, want 42", got)
	}
	if !resumed {
		t.Fatal("the pause handler never ran: the breakpoint did not pause")
	}

	var paused map[string]interface{}
	for _, n := range notifications {
		if n["method"] == "Debugger.paused" {
			paused = n
			break
		}
	}
	if paused == nil {
		t.Fatal("no Debugger.paused event arrived")
	}
	params, _ := paused["params"].(map[string]interface{})
	frames, _ := params["callFrames"].([]interface{})
	if len(frames) < 1 {
		t.Fatalf("the paused event carried %d call frames, want at least 1", len(frames))
	}
	top, _ := frames[0].(map[string]interface{})
	if name, _ := top["functionName"].(string); name != "target" {
		t.Errorf("paused in %q, want target", name)
	}
}

// Runtime.evaluate over the session: the console/preview half's transport.
func TestInspectorRuntimeEvaluate(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	inspector := v8.NewInspector(iso)
	defer inspector.Dispose()
	inspector.ContextCreated(ctx)
	session := inspector.Connect()
	defer session.Dispose()

	var responses []map[string]interface{}
	contextID := float64(0)
	session.Message = func(callID int, message []byte) {
		var parsed map[string]interface{}
		if err := json.Unmarshal(message, &parsed); err != nil {
			return
		}
		if callID < 0 {
			// Runtime.enable announces the contexts the inspector knows; the
			// id it hands out is what evaluate targets.
			if parsed["method"] == "Runtime.executionContextCreated" {
				params, _ := parsed["params"].(map[string]interface{})
				desc, _ := params["context"].(map[string]interface{})
				contextID, _ = desc["id"].(float64)
			}
			return
		}
		responses = append(responses, parsed)
	}
	session.DispatchProtocolMessage([]byte(`{"id":1,"method":"Runtime.enable","params":{}}`))
	if contextID == 0 {
		t.Fatal("Runtime.enable announced no execution context")
	}
	session.DispatchProtocolMessage([]byte(fmt.Sprintf(
		`{"id":2,"method":"Runtime.evaluate","params":{"expression":"6*7","contextId":%d}}`,
		int(contextID))))

	if len(responses) != 2 {
		t.Fatalf("%d responses, want 2", len(responses))
	}
	result, _ := responses[1]["result"].(map[string]interface{})
	inner, _ := result["result"].(map[string]interface{})
	if got, _ := inner["value"].(float64); got != 42 {
		t.Errorf("Runtime.evaluate answered %v, want 42", inner)
	}
}

// A breakpoint no script line matches is a protocol answer, not a panic, and
// a script that never reaches a breakpoint never pauses.
func TestInspectorBreakpointOnNothing(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	inspector := v8.NewInspector(iso)
	defer inspector.Dispose()
	inspector.ContextCreated(ctx)
	session := inspector.Connect()
	defer session.Dispose()
	session.Message = func(int, []byte) {}

	session.DispatchProtocolMessage([]byte(`{"id":1,"method":"Debugger.enable","params":{}}`))
	session.DispatchProtocolMessage([]byte(
		`{"id":2,"method":"Debugger.setBreakpointByUrl","params":{"lineNumber":9999,"url":"nowhere.js"}}`))

	paused := false
	inspector.PauseHandler = func() bool {
		paused = true
		session.DispatchProtocolMessage([]byte(`{"id":3,"method":"Debugger.resume","params":{}}`))
		return true
	}
	if _, err := ctx.RunScript("1 + 1", "other.js"); err != nil {
		t.Fatalf("the script failed: %v", err)
	}
	if paused {
		t.Error("a breakpoint on a line that does not exist paused execution")
	}
}
