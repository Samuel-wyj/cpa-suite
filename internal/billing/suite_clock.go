package billing

import "time"

// SetClockForTest replaces the store clock. CPA Suite parity tests use it to
// replay one scripted scenario against upstream and suite builds with
// identical timestamps. Production code never calls it; call it before any
// concurrent use of the store.
func (s *Store) SetClockForTest(now func() time.Time) {
	s.now = now
}
