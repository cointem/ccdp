//go:build darwin && cgo

package computer

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -fblocks
#cgo LDFLAGS: -framework Cocoa -framework ApplicationServices -framework ScreenCaptureKit
#include <stdlib.h>
char *ccdp_computer(const char *request);
*/
import "C"

import (
	"encoding/json"
	"unsafe"
)

func nativeCall(request map[string]any) Response {
	data, err := json.Marshal(request)
	if err != nil {
		return Response{Error: err.Error()}
	}
	input := C.CString(string(data))
	defer C.free(unsafe.Pointer(input))
	output := C.ccdp_computer(input)
	if output == nil {
		return Response{Error: "native Computer driver returned no result"}
	}
	defer C.free(unsafe.Pointer(output))
	var response Response
	if err := json.Unmarshal([]byte(C.GoString(output)), &response); err != nil {
		return Response{Error: err.Error()}
	}
	return response
}
