package engine

// SetClockForTest fixes the virtual clock (deterministic tests).
func (e *Engine) SetClockForTest(ms int64) {
	e.mu.Lock()
	e.nowMS = ms
	e.mu.Unlock()
	e.rearmProbesFromDisk()
}

// ClockForTest returns the current virtual time.
func (e *Engine) ClockForTest() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.nowMS
}
