package api

// RunTailers is how many run tailers h has running.
func (h *Hub) RunTailers() int64 { return h.runs.live.Load() }
