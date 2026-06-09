// Package clock provides virtual time sources for the control plane.
// The virtual clock never reads host time, ensuring deterministic scheduling
// regardless of real-world execution speed.
package clock

import "time"

// Clock reads the current time. Implementations may be real or virtual.
type Clock interface {
	Now() time.Time
}

// Virtual is a monotonic virtual clock controlled by the simulation.
// It never reads host time. Panics if AdvanceTo is called with a time before Now().
type Virtual struct {
	now time.Time
}

// NewVirtual returns a Virtual clock starting at the given epoch.
func NewVirtual(epoch time.Time) *Virtual {
	return &Virtual{now: epoch}
}

// Now returns the current virtual time.
func (c *Virtual) Now() time.Time {
	return c.now
}

// Advance moves the virtual clock forward by d.
func (c *Virtual) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}

// AdvanceTo sets the virtual clock to t. It panics if t is before the current time
// because backwards time travel would break causal ordering of events.
func (c *Virtual) AdvanceTo(t time.Time) {
	if t.Before(c.now) {
		panic("clock: cannot advance to a time before Now()")
	}
	c.now = t
}
