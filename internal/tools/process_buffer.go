package tools

// processBuffer retains bytes without reallocating on each output fragment.
// Its owner holds managedProcess.mu; readers receive independent snapshots.
type processBuffer struct {
	data       []byte
	next, size int
}

func (b *processBuffer) Len() int { return b.size }

func (b *processBuffer) Write(p []byte) {
	if len(p) == 0 {
		return
	}
	if b.data == nil {
		b.data = make([]byte, maxProcOutput)
	}
	if len(p) >= len(b.data) {
		copy(b.data, p[len(p)-len(b.data):])
		b.next, b.size = 0, len(b.data)
		return
	}
	n := copy(b.data[b.next:], p)
	copy(b.data, p[n:])
	b.next = (b.next + len(p)) % len(b.data)
	b.size = min(len(b.data), b.size+len(p))
}

func (b *processBuffer) Tail(limit int) []byte {
	n := min(max(0, limit), b.size)
	if n == 0 {
		return nil
	}
	out := make([]byte, n)
	start := (b.next - n + len(b.data)) % len(b.data)
	first := copy(out, b.data[start:])
	copy(out[first:], b.data[:n-first])
	return out
}
