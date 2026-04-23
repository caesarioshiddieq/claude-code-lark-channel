package worker

// BusySnapshot returns current busy task IDs for white-box pool tests.
func (p *Pool) BusySnapshot() []string {
	return p.snapshotBusy()
}
