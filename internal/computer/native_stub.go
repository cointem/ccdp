//go:build !darwin || !cgo

package computer

func nativeCall(map[string]any) Response {
	return Response{Status: &Status{}, Error: "Computer requires macOS 14 or later and a build with CGO enabled"}
}
