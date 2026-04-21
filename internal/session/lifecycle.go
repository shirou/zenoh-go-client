package session

// RunLoops starts the session's long-running goroutines.
//
// Each spawned goroutine must:
//   - register with s.wg before entering its loop,
//   - recover() any panic and log via s.Logger(),
//   - return once s.closing is closed.
//
// The current implementation launches no goroutines; concrete reader /
// writer / keepalive / lease-watchdog / dispatcher / reconnect-manager
// loops will attach here.
func (s *Session) RunLoops() {}
