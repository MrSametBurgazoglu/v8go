// Copyright 2019 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go

// #include <stdlib.h>
// #include "v8go.h"
import "C"

import (
	"runtime/cgo"
	"unsafe"
)

// Inspector is the v8-inspector attached to one isolate: the machinery behind
// the Chrome DevTools Protocol's Runtime, Debugger, Profiler and HeapProfiler
// domains, compiled into libv8 all along and reachable from Go only now.
//
// One Inspector per isolate; one or more Sessions per Inspector. Everything
// here must run on the isolate's own thread — the same rule as every other
// call into V8 — including DispatchProtocolMessage. The exception is designed
// in: while the debugger is PAUSED inside a dispatched message, V8 keeps the
// thread and calls PauseHandler; dispatching further messages from inside
// that handler is the protocol's intended shape and is how stepping works.
type Inspector struct {
	ptr    C.InspectorPtr
	iso    *Isolate
	handle cgo.Handle

	// PauseHandler runs while the debugger is paused at a breakpoint. It must
	// block until it has a protocol message to dispatch (Debugger.resume, a
	// step, an evaluate-on-call-frame …), dispatch it via the session, and
	// return true to stay in the pause loop — the loop exits by itself once a
	// dispatched message resumed execution. Returning false abandons the pause
	// (session shutdown). A nil handler resumes immediately, which turns every
	// breakpoint into a no-op rather than a hang.
	PauseHandler func() bool
}

// NewInspector attaches an inspector to the isolate. The isolate's internal
// context (SetIsolateInternalContext) is what Runtime.evaluate runs against
// when the frontend does not name one.
func NewInspector(iso *Isolate) *Inspector {
	insp := &Inspector{iso: iso}
	insp.handle = cgo.NewHandle(insp)
	insp.ptr = C.NewInspector(iso.ptr, C.uintptr_t(insp.handle))
	return insp
}

// ContextCreated announces a context to the inspector. Scripts compiled before
// this call have no debugger metadata, so announce the context before running
// anything that should be debuggable.
func (i *Inspector) ContextCreated(ctx *Context) {
	C.InspectorContextCreated(i.ptr, ctx.ptr)
}

// ContextDestroyed retracts a context on navigation.
func (i *Inspector) ContextDestroyed(ctx *Context) {
	C.InspectorContextDestroyed(i.ptr, ctx.ptr)
}

// Dispose frees the inspector. Sessions must be disposed first.
func (i *Inspector) Dispose() {
	if i.ptr == nil {
		return
	}
	C.InspectorDispose(i.ptr)
	i.ptr = nil
	i.handle.Delete()
}

// InspectorSession is one protocol connection: JSON in through
// DispatchProtocolMessage, JSON out through the Message callback.
type InspectorSession struct {
	ptr       C.InspectorSessionPtr
	inspector *Inspector
	handle    cgo.Handle

	// Message receives every outbound protocol message. callID is the id of
	// the command a response answers, and -1 for a notification (an event).
	// Called synchronously from inside V8 — often from within a
	// DispatchProtocolMessage call — so it must not call back into the
	// session or the isolate.
	Message func(callID int, message []byte)
}

// Connect opens a session on the inspector.
func (i *Inspector) Connect() *InspectorSession {
	s := &InspectorSession{inspector: i}
	s.handle = cgo.NewHandle(s)
	s.ptr = C.InspectorConnect(i.ptr, C.uintptr_t(s.handle))
	return s
}

// DispatchProtocolMessage hands one frontend command (UTF-8 JSON) to the
// session. Responses and any events it provokes arrive through Message before
// this returns; a command that pauses the debugger (or runs into a breakpoint)
// does not return until the pause ends.
func (s *InspectorSession) DispatchProtocolMessage(message []byte) {
	if len(message) == 0 {
		return
	}
	data := C.CBytes(message)
	defer C.free(data)
	C.InspectorSessionDispatch(s.ptr, (*C.char)(data), C.int(len(message)))
}

// Dispose closes the session.
func (s *InspectorSession) Dispose() {
	if s.ptr == nil {
		return
	}
	C.InspectorSessionDispose(s.ptr)
	s.ptr = nil
	s.handle.Delete()
}

//export goInspectorMessage
func goInspectorMessage(ref C.uintptr_t, callID C.int, data *C.char, length C.int) {
	handle := cgo.Handle(ref)
	session, ok := handle.Value().(*InspectorSession)
	if !ok || session.Message == nil {
		return
	}
	session.Message(int(callID), C.GoBytes(unsafe.Pointer(data), length))
}

//export goInspectorPauseTick
func goInspectorPauseTick(ref C.uintptr_t) C.int {
	handle := cgo.Handle(ref)
	inspector, ok := handle.Value().(*Inspector)
	if !ok || inspector.PauseHandler == nil {
		return 0 // no handler: abandon the pause rather than hang the isolate
	}
	if inspector.PauseHandler() {
		return 1
	}
	return 0
}
