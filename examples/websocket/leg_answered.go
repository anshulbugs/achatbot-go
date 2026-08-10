package main

import "sync"

// Waiting for a dialled leg to be answered.
//
// The listen-in SIP leg is a call we PLACE, not a call we own: it never enters
// the call registry, and until now nothing tracked it. That left the barge path
// guessing how long Daily takes to answer, with a flat settle that was three
// and a half of the eight seconds an operator waited — all of it with the agent
// still on the call.
//
// The leg is dialled with our own webhook URL, so its `call.answered` arrives
// at the ordinary Telnyx handler. This is the meeting point between that
// handler and the goroutine waiting to take the agent off the call.
var (
	legMu       sync.Mutex
	legAnswered = map[string]chan struct{}{}
)

// watchLegAnswered registers interest in a leg and returns the channel closed
// when it answers. Register BEFORE anything can answer, or the signal is missed.
func watchLegAnswered(legID string) <-chan struct{} {
	ch := make(chan struct{})
	if legID == "" {
		return ch // never fires; the caller's timeout covers it
	}
	legMu.Lock()
	// A repeat dial of the same id would otherwise strand the first waiter.
	if existing, ok := legAnswered[legID]; ok {
		legMu.Unlock()
		return existing
	}
	legAnswered[legID] = ch
	legMu.Unlock()
	return ch
}

// signalLegAnswered reports that a dialled leg has been answered. Safe to call
// for every call.answered event: an id nobody is waiting on does nothing.
func signalLegAnswered(legID string) {
	legMu.Lock()
	ch, ok := legAnswered[legID]
	if ok {
		delete(legAnswered, legID)
	}
	legMu.Unlock()
	if ok {
		close(ch)
	}
}

// forgetLeg drops a registration whose waiter has given up, so a leg that never
// answers cannot leak an entry for the life of the process.
func forgetLeg(legID string) {
	legMu.Lock()
	delete(legAnswered, legID)
	legMu.Unlock()
}
