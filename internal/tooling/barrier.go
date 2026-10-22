package tooling

// Barrier is the shared admission rule for static batches and streaming code
// cells. Its owner serializes access. Unknown/mutating calls are exclusive;
// consecutive trusted reads may share up to Limit permits.
type Barrier struct {
	Limit     int
	Active    int
	exclusive bool
}

func (b *Barrier) CanStart(readOnly bool) bool {
	return !b.exclusive && b.Active < max(1, b.Limit) && (readOnly || b.Active == 0)
}
func (b *Barrier) Start(readOnly bool) {
	b.Active++
	if !readOnly {
		b.exclusive = true
	}
}
func (b *Barrier) Finish(readOnly bool) {
	b.Active--
	if !readOnly {
		b.exclusive = false
	}
}
