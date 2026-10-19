package v8go

// #include <stdlib.h>
// #include "v8go.h"
import "C"
import (
	"runtime"
	"sync"
	"unsafe"
)

// ScriptStream is a classic script being parsed and compiled off the
// isolate's thread (V8's script streaming): the compile Chromium overlaps
// with the download, here started once the source is in hand and finished
// by Compile on the isolate's thread.
//
// Start, Compile and Close are the isolate's thread's; the background half
// runs on a goroutine of its own. A stream must be compiled or closed before
// its isolate is disposed.
type ScriptStream struct {
	ptr  C.ScriptStreamPtr
	iso  *Isolate
	done chan struct{}
	once sync.Once

	// An incremental stream's pushed bytes not yet handed to V8, coalesced
	// so no chunk splits a UTF-8 sequence over more than two (V8's limit).
	mu          sync.Mutex
	pending     []byte
	incremental bool
	finished    bool
}

// pushThreshold is the size pushed chunks are coalesced to.
const pushThreshold = 16 << 10

// StartScriptStream begins streaming source. Call Compile with the same
// source to get the script, or Close to abandon it.
func (i *Isolate) StartScriptStream(source string) *ScriptStream {
	// Copied again by the C side, which owns the bytes V8 parses.
	data := C.CString(source)
	defer C.free(unsafe.Pointer(data))
	s := &ScriptStream{
		ptr:  C.IsolateStartScriptStream(i.ptr, data, C.int(len(source))),
		iso:  i,
		done: make(chan struct{}),
	}
	s.run()
	return s
}

// StartIncrementalScriptStream begins streaming a source that is not yet
// all here: Push its bytes as they arrive, from any goroutine, then Finish.
// V8 parses what it has been given on a background thread meanwhile.
// Compile with the whole source, once it is in hand.
func (i *Isolate) StartIncrementalScriptStream() *ScriptStream {
	s := &ScriptStream{
		ptr:         C.IsolateStartIncrementalScriptStream(i.ptr),
		iso:         i,
		done:        make(chan struct{}),
		incremental: true,
	}
	s.run()
	return s
}

func (s *ScriptStream) run() {
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		C.ScriptStreamRun(s.ptr)
		close(s.done)
	}()
}

// Push hands the stream the next bytes of its source. Any goroutine.
func (s *ScriptStream) Push(chunk []byte) {
	if !s.incremental || len(chunk) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.pending = append(s.pending, chunk...)
	if len(s.pending) >= pushThreshold {
		s.flushLocked()
	}
}

func (s *ScriptStream) flushLocked() {
	if len(s.pending) == 0 {
		return
	}
	C.ScriptStreamPush(s.ptr, (*C.char)(unsafe.Pointer(&s.pending[0])), C.int(len(s.pending)))
	s.pending = s.pending[:0]
}

// Finish says the source is complete. Any goroutine; idempotent.
func (s *ScriptStream) Finish() {
	if !s.incremental {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.flushLocked()
	s.pending = nil
	s.finished = true
	C.ScriptStreamFinish(s.ptr)
}

// Done is closed when the background half has finished.
func (s *ScriptStream) Done() <-chan struct{} { return s.done }

// Compile waits for the background half and compiles the streamed script,
// with source the text that was streamed. Only Opaque of opts is read.
// The stream is released either way.
func (s *ScriptStream) Compile(source, origin string, opts CompileOptions) (*UnboundScript, error) {
	s.Finish()
	<-s.done
	cSource := C.CString(source)
	cOrigin := C.CString(origin)
	defer C.free(unsafe.Pointer(cSource))
	defer C.free(unsafe.Pointer(cOrigin))
	opaque := C.int(0)
	if opts.Opaque {
		opaque = 1
	}
	rtn := C.ScriptStreamCompile(s.iso.ptr, s.ptr, cSource, C.int(len(source)), cOrigin, opaque)
	s.Close()
	if rtn.ptr == nil {
		return nil, newJSError(rtn.error)
	}
	return &UnboundScript{ptr: rtn.ptr, iso: s.iso}, nil
}

// Close finishes the source, waits for the background half and releases
// the stream.
func (s *ScriptStream) Close() {
	s.Finish()
	<-s.done
	s.once.Do(func() {
		C.ScriptStreamFree(s.ptr)
		s.ptr = nil
	})
}
