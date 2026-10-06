// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasip1

package runtime

import _ "unsafe" // for go:linkname

// wasm has no support for threads yet. There is no preemption.
// See proposal: https://github.com/WebAssembly/threads
// Waiting for a mutex or timeout is implemented as a busy loop
// while allowing other goroutines to run.

const (
	mutex_unlocked = 0
	mutex_locked   = 1

	active_spin     = 4
	active_spin_cnt = 30

	mutexMLocksDelta = 16
)

type mWaitList struct{}

func lockVerifyMSize() {}

func mutexContended(l *mutex) bool {
	return false
}

func lock(l *mutex) {
	lockWithRank(l, getLockRank(l))
}

func lock2(l *mutex) {
	if l.key == mutex_locked {
		// wasm is single-threaded so we should never
		// observe this.
		throw("self deadlock")
	}
	gp := getg()
	if gp.m.locks < 0 {
		throw("lock count")
	}
	gp.m.locks += mutexMLocksDelta
	l.key = mutex_locked
}

func unlock(l *mutex) {
	unlockWithRank(l)
}

func unlock2(l *mutex) {
	if l.key == mutex_unlocked {
		throw("unlock of unlocked lock")
	}
	gp := getg()
	gp.m.locks -= mutexMLocksDelta
	if gp.m.locks < 0 {
		throw("lock count")
	}
	l.key = mutex_unlocked
}

// One-time notifications.
func noteclear(n *note) {
	n.key = 0
}

func notewakeup(n *note) {
	if n.key != 0 {
		print("notewakeup - double wakeup (", n.key, ")\n")
		throw("notewakeup - double wakeup")
	}
	n.key = 1
}

func notesleep(n *note) {
	throw("notesleep not supported by wasi")
}

func notetsleep(n *note, ns int64) bool {
	throw("notetsleep not supported by wasi")
	return false
}

// same as runtime·notetsleep, but called on user g (not g0)
func notetsleepg(n *note, ns int64) bool {
	gp := getg()
	if gp == gp.m.g0 {
		throw("notetsleepg on g0")
	}

	deadline := nanotime() + ns
	for {
		if n.key != 0 {
			return true
		}
		if sched_yield() != 0 {
			throw("sched_yield failed")
		}
		Gosched()
		if ns >= 0 && nanotime() >= deadline {
			return false
		}
	}
}

var onIdle = func() bool {
	return false
}

func wasiOnIdle(callback func() bool) {
	onIdle = callback
}

// idlePollUntil is when the next program timer is due (0 when none is) at the
// latest idle transition, for the onIdle callback to read with wasiIdleTimer.
var idlePollUntil int64

func beforeIdle(now int64, pollUntil int64, netWaiters bool) (*g, bool) {
	// No clock is read here: an idle transition is exactly where a host may
	// suspend the component, and a clock read there keeps a durable host from
	// treating it as parked.
	idlePollUntil = 0
	if pollUntil != 0 {
		idlePollUntil = nextProgramTimer()
	}
	return nil, !netWaiters && onIdle()
}

// nextProgramTimer is when the earliest timer the program itself set is due,
// or 0 when none is pending. The background scavenger's sleep is left out: it
// is housekeeping that can wait until the component runs again, and a host
// clock wait armed for it would keep an otherwise idle component from ever
// suspending.
func nextProgramTimer() int64 {
	var next int64
	for _, pp := range allp {
		ts := &pp.timers
		lock(&ts.mu)
		for _, tw := range ts.heap {
			if tw.timer == scavenger.timer {
				continue
			}
			if tw.when != 0 && (next == 0 || tw.when < next) {
				next = tw.when
			}
		}
		unlock(&ts.mu)
	}
	return next
}

// wasiIdleTimer reports, to an onIdle callback, when the next program timer
// is due (0 when none is) and the monotonic clock reading to measure the delay
// from. The clock is read only when a timer is pending. A callback that hands
// control to the host must arrange to be resumed by then, or the timer cannot
// fire.
//
//go:linkname wasiIdleTimer
func wasiIdleTimer() (now int64, pollUntil int64) {
	if idlePollUntil == 0 {
		return 0, 0
	}
	return nanotime(), idlePollUntil
}

func checkTimeouts() {}

//go:wasmimport wasi_snapshot_preview1 sched_yield
func sched_yield() errno
