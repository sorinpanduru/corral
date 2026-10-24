package errclass

// panicError is the error returned by FromPanic. It deliberately holds no
// reference to the recovered value (FR-24).
type panicError struct{ stack []byte }

func (*panicError) Error() string { return "recovered panic" }

// FromPanic converts a recovered value into an internal error. The value is
// discarded; only the optional stack is kept. Error() is a constant.
func FromPanic(recovered any, stack []byte) error {
	_ = recovered // compliance: never retained or formatted (FR-24)
	p := &panicError{}
	if len(stack) > 0 {
		p.stack = append([]byte(nil), stack...)
	}
	return p
}

// Stack returns the stack kept by FromPanic, if any.
func Stack(err error) ([]byte, bool) {
	for _, n := range walk(err) {
		if p, ok := n.(*panicError); ok && len(p.stack) > 0 {
			return append([]byte(nil), p.stack...), true
		}
	}
	return nil, false
}
