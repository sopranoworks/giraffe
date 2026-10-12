// Package crash lets a panic in any goroutine reach the one handler main
// registers, so it can be recorded (directive 087 §10) before the
// process ends as it always did. recover only works in the goroutine
// that panicked, so every goroutine giraffe starts defers Handle.
package crash

import (
	"runtime/debug"
	"strings"
)

var handler func(v any, stack []byte)

// SetHandler registers what a recovered panic is given to. The handler
// is expected to end the process.
func SetHandler(h func(v any, stack []byte)) { handler = h }

// Handle is deferred at the top of a goroutine. With no handler
// registered the panic goes on, as if it had not been recovered.
func Handle() {
	if r := recover(); r != nil {
		if handler == nil {
			panic(r)
		}
		handler(r, trim(debug.Stack()))
	}
}

// trim drops the frames of the recovery itself — Stack, Handle and the
// runtime's panic — leaving the goroutine's header and the frames from
// the panicking call, as an unrecovered panic prints them.
func trim(stack []byte) []byte {
	lines := strings.Split(string(stack), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "panic(") && i+2 <= len(lines) {
			return []byte(lines[0] + "\n" + strings.Join(lines[i+2:], "\n"))
		}
	}
	return stack
}
