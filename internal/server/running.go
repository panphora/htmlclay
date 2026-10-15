package server

// RunningWork counts the AI edits and helper program requests in flight. An
// update restart stops all of them, so the app asks before it restarts while
// this is above zero.
func (s *Server) RunningWork() int {
	s.aiEdit.mu.Lock()
	n := len(s.aiEdit.running)
	s.aiEdit.mu.Unlock()

	s.helperMu.Lock()
	dispatchers := make([]*helperDispatcher, 0, len(s.helpers))
	for _, d := range s.helpers {
		dispatchers = append(dispatchers, d)
	}
	s.helperMu.Unlock()
	for _, d := range dispatchers {
		d.mu.Lock()
		n += len(d.live)
		d.mu.Unlock()
	}
	return n
}
