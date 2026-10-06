package v8go

// #include <stdlib.h>
// #include "v8go.h"
import "C"
import (
	"errors"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

// StringWTF8 is String for a value whose UTF-16 may hold unpaired
// surrogates: each is written as its own three-byte sequence (WTF-8) rather
// than replaced with U+FFFD, so NewStringWTF8 gives back the same JS string.
// A well-formed string reads exactly as String reads it. A value whose
// conversion to a string throws reads as "".
func (v *Value) StringWTF8() string {
	rtn := C.ValueToWTF8String(v.ptr)
	if rtn.data == nil {
		_ = newJSError(rtn.error) // frees what the failed conversion reported
		return ""
	}
	defer C.free(unsafe.Pointer(rtn.data))
	return C.GoStringN(rtn.data, rtn.length)
}

// NewStringWTF8 makes a JS string from WTF-8: UTF-8 in which a three-byte
// sequence may encode a lone surrogate, as StringWTF8 writes one. Any other
// invalid byte becomes U+FFFD, as NewValue does for a string.
func NewStringWTF8(iso *Isolate, s string) (*Value, error) {
	if iso == nil {
		return nil, errors.New("v8go: failed to create new Value: Isolate cannot be <nil>")
	}
	units := wtf8ToUTF16(s)
	var ptr *C.uint16_t
	if len(units) > 0 {
		ptr = (*C.uint16_t)(unsafe.Pointer(&units[0]))
	}
	rtn := C.NewValueStringUTF16(iso.ptr, ptr, C.int(len(units)))
	return valueResult(nil, rtn)
}

func wtf8ToUTF16(s string) []uint16 {
	units := make([]uint16, 0, len(s))
	for i := 0; i < len(s); {
		// ED A0..BF 80..BF is a surrogate's three-byte form, which
		// utf8.DecodeRuneInString refuses.
		if i+2 < len(s) && s[i] == 0xED && s[i+1] >= 0xA0 && s[i+1] <= 0xBF &&
			s[i+2] >= 0x80 && s[i+2] <= 0xBF {
			units = append(units, 0xD000|uint16(s[i+1]&0x3F)<<6|uint16(s[i+2]&0x3F))
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r >= 0x10000 {
			hi, lo := utf16.EncodeRune(r)
			units = append(units, uint16(hi), uint16(lo))
		} else {
			units = append(units, uint16(r))
		}
		i += size
	}
	return units
}
