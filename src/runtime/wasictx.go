// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/abi"
	"internal/chacha8rand"
	"internal/stringslite"
	"unsafe"
)

// Goroutine-local host execution context and runtime state domains on wasip1.
//
// A WASI host may keep execution context per component task, while every
// goroutine of the program shares that task. A program that wants each
// goroutine to run with its own host context gives the goroutine an opaque
// value (g.wasiCtx) and registers hooks:
//
//   - switch(from, to) runs on the system stack whenever the goroutine about to
//     run has a different value than the one installed last. It must not
//     allocate, block or grow the stack; it may call host imports.
//   - create(parent) runs at a go statement when the parent has a value, before
//     the child exists; its result becomes the child's value. It runs on the
//     parent goroutine, or on the system stack for a goroutine that a timer
//     function starts (time.AfterFunc).
//   - exit(ctx) runs on an exiting goroutine that has a value, after the nil
//     value has been installed.
//
// A durable host may also skip a part of the program on replay and return the
// result it recorded instead. Runtime state that every goroutine shares would
// then carry the effects of the skipped part into the rest of the program. A
// program can therefore run a part of itself in its own domain
// (wasiDomainEnter). A domain has its own:
//
//   - clock: the scheduler fires a domain's timers against the latest monotonic
//     reading made in the domain, or the due time of a host clock wait that the
//     idle callback armed for the domain (wasiIdleWait, wasiAdvanceTime);
//   - timers heap;
//   - random state, which map seeds and iteration, select and math/rand draw
//     from. A new domain is seeded with draws from its parent's state, so the
//     parent's stream advances by the same amount whether or not the part that
//     runs in the new domain is skipped.
//
// Goroutines inherit their parent's domain; goroutines that a timer function
// starts get the timer's domain. When a domain exits, its timers are stopped.
// The runtime's own goroutines (GC workers, the finalizer and cleanup
// goroutines) and the runtime's own work on the system stack, under a runtime
// lock or inside an allocation draw from the runtime's random state and read
// the clock that the scheduler keeps for itself, which is the latest reading
// any goroutine made. The runtime makes no host call for itself.
//
// Coroutines (iter.Pull) carry their creator's value and domain but run in
// whatever host context is installed by the goroutine that resumes them. They,
// and the goroutines the runtime starts for itself, are not reported to the
// create and exit hooks.

var (
	wasiCtxInstalled  unsafe.Pointer
	wasiCtxSwitchHook func(from, to unsafe.Pointer)
	wasiCtxCreateHook func(parent unsafe.Pointer) unsafe.Pointer
	wasiCtxExitHook   func(ctx unsafe.Pointer)
)

// wasiSetContextHooks registers the hooks; see above.
//
//go:linkname wasiSetContextHooks
func wasiSetContextHooks(switchFn func(from, to unsafe.Pointer), create func(parent unsafe.Pointer) unsafe.Pointer, exit func(ctx unsafe.Pointer)) {
	wasiCtxSwitchHook = switchFn
	wasiCtxCreateHook = create
	wasiCtxExitHook = exit
}

// wasiGetContext returns the calling goroutine's value.
//
//go:linkname wasiGetContext
func wasiGetContext() unsafe.Pointer {
	return getg().m.curg.wasiCtx
}

// wasiSetContext sets the calling goroutine's value and records it as
// installed: the caller has already put the host into that context.
//
//go:linkname wasiSetContext
func wasiSetContext(ctx unsafe.Pointer) {
	getg().m.curg.wasiCtx = ctx
	wasiCtxInstalled = ctx
}

// wasiInstallContext installs ctx through the switch hook. A goroutine that
// installs another value must install its own again before it blocks.
//
//go:linkname wasiInstallContext
func wasiInstallContext(ctx unsafe.Pointer) {
	wasiCtxInstall(ctx)
}

// wasiCtxInstall is called by the scheduler with a P held, so write barriers
// are allowed.
//
//go:yeswritebarrierrec
func wasiCtxInstall(ctx unsafe.Pointer) {
	if ctx == wasiCtxInstalled || wasiCtxSwitchHook == nil {
		return
	}
	wasiCtxSwitchHook(wasiCtxInstalled, ctx)
	wasiCtxInstalled = ctx
}

// wasiForChild returns the value and the domain of a goroutine that parent is
// about to start, running the create hook before the child exists, so the
// child is never runnable without its own value. Goroutines of the runtime
// itself outlive the goroutine that happens to start them and get no value and
// the runtime's domain. A goroutine started on the system stack by a timer
// function gets the timer's domain, and a value created from the domain's
// context.
func wasiForChild(parent *g, fn *funcval) (unsafe.Pointer, *wasiDomain) {
	if wasiRuntimeGoroutine(fn) {
		return nil, &wasiRuntimeDomain
	}
	ctx, dom := parent.wasiCtx, parent.wasiDom
	if parent != parent.m.curg {
		ctx, dom = nil, nil
		if wasiFiring {
			dom = wasiFiringDom
			if d := dom; d != nil && d != &wasiRuntimeDomain && d.ctx != nil {
				ctx = d.ctx
				wasiCtxInstall(ctx)
			}
		}
	}
	if ctx == nil || wasiCtxCreateHook == nil {
		return nil, dom
	}
	return wasiCtxCreateHook(ctx), dom
}

// wasiRuntimeGoroutine reports whether fn starts a goroutine of the runtime,
// following isSystemGoroutine.
func wasiRuntimeGoroutine(fn *funcval) bool {
	f := findfunc(fn.fn)
	if !f.valid() {
		return false
	}
	switch f.funcID {
	case abi.FuncID_runtime_main, abi.FuncID_corostart, abi.FuncID_handleAsyncEvent:
		return false
	}
	return stringslite.HasPrefix(funcname(f), "runtime.")
}

func wasiCtxExited(gp *g) {
	ctx := gp.wasiCtx
	if ctx == nil {
		return
	}
	wasiCtxInstall(nil)
	gp.wasiCtx = nil
	if wasiCtxExitHook != nil {
		wasiCtxExitHook(ctx)
	}
}

// A wasiDomain is the clock, timers and random state of one part of the
// program; see above. A nil g.wasiDom or timer.wasiDom is the root domain, and
// wasiRuntimeDomain marks the runtime's own goroutines and timers.
type wasiDomain struct {
	// parent is the value of g.wasiDom that the entering goroutine had.
	parent *wasiDomain
	// ctx is the host context that the domain's clock waits run in.
	ctx unsafe.Pointer
	// now is the latest monotonic reading made in the domain.
	now int64
	// idleDue is when the domain's next timer is due (0 when none is), as of
	// the latest idle transition.
	idleDue int64
	open    bool

	timers    timers
	chacha8   chacha8rand.State
	cheaprand uint32
}

var (
	wasiRootDomain    wasiDomain
	wasiRuntimeDomain wasiDomain
	// wasiDomains are the open domains, the root first.
	wasiDomains []*wasiDomain

	// wasiFiring and wasiFiringDom report the domain of the timer whose
	// function runs.
	wasiFiring    bool
	wasiFiringDom *wasiDomain
)

// wasiDomainInit seeds the root domain from the runtime's random state, right
// after the runtime seeded that state.
func wasiDomainInit() {
	d := &wasiRootDomain
	lockInit(&d.timers.mu, lockRankTimers)
	d.seed()
	d.open = true
	wasiDomains = append(wasiDomains, d)
}

func (d *wasiDomain) seed() {
	var seed [4]uint64
	for i := range seed {
		seed[i] = rand()
	}
	d.chacha8.Init64(seed)
	d.cheaprand = uint32(rand())
}

// wasiDomainOf maps a g.wasiDom or timer.wasiDom value to its domain, or nil
// for the runtime's own.
//
//go:nosplit
func wasiDomainOf(d *wasiDomain) *wasiDomain {
	if d == nil {
		return &wasiRootDomain
	}
	if d == &wasiRuntimeDomain {
		return nil
	}
	return d
}

// wasiRandDomain returns the domain whose random state the caller draws from,
// or nil for the runtime's own state.
//
//go:nosplit
func wasiRandDomain() *wasiDomain {
	gp := getg()
	mp := gp.m
	if gp != mp.curg || mp.locks != 0 || mp.mallocing != 0 {
		return nil
	}
	return wasiDomainOf(gp.wasiDom)
}

// wasiDomainEnter runs the calling goroutine in a new domain, a child of its
// current one. Its clock starts at the parent's, and its clock waits run in
// host context ctx until wasiDomainSetContext changes it.
//
//go:linkname wasiDomainEnter
func wasiDomainEnter(ctx unsafe.Pointer) unsafe.Pointer {
	gp := getg().m.curg
	d := new(wasiDomain)
	lockInit(&d.timers.mu, lockRankTimers)
	d.parent = gp.wasiDom
	d.ctx = ctx
	if p := wasiDomainOf(gp.wasiDom); p != nil {
		d.now = p.now
	} else {
		d.now = wasiLastNanotime
	}
	d.seed()
	d.open = true
	wasiDomains = append(wasiDomains, d)
	gp.wasiDom = d
	return unsafe.Pointer(d)
}

// wasiDomainSetContext sets the host context that the domain's clock waits,
// and the goroutines its timer functions start, run in.
//
//go:linkname wasiDomainSetContext
func wasiDomainSetContext(dom, ctx unsafe.Pointer) {
	(*wasiDomain)(dom).ctx = ctx
}

// wasiDomainStop stops every timer of the domain.
//
//go:linkname wasiDomainStop
func wasiDomainStop(dom unsafe.Pointer) {
	ts := &(*wasiDomain)(dom).timers
	for i := 0; i < len(ts.heap); i++ {
		ts.heap[i].timer.stop()
	}
	ts.lock()
	for _, tw := range ts.heap {
		t := tw.timer
		t.lock()
		t.ts = nil
		t.state &^= timerHeaped | timerZombie | timerModified
		t.unlock()
	}
	ts.heap = nil
	ts.zombies.Store(0)
	ts.minWhenHeap.Store(0)
	ts.minWhenModified.Store(0)
	ts.len.Store(0)
	ts.unlock()
}

// wasiDomainExit stops the domain's timers and runs the calling goroutine in
// the domain it entered from.
//
//go:linkname wasiDomainExit
func wasiDomainExit(dom unsafe.Pointer) {
	d := (*wasiDomain)(dom)
	wasiDomainStop(dom)
	d.open = false
	for i, e := range wasiDomains {
		if e == d {
			last := len(wasiDomains) - 1
			copy(wasiDomains[i:], wasiDomains[i+1:])
			wasiDomains[last] = nil
			wasiDomains = wasiDomains[:last]
			break
		}
	}
	getg().m.curg.wasiDom = d.parent
}

// wasiTimersOf returns the heap that t joins: its domain's, or the P's for the
// runtime's own timers. A timer of a domain that has exited joins the current
// goroutine's domain.
func wasiTimersOf(t *timer, pp *p) *timers {
	if t.wasiDom != nil && t.wasiDom != &wasiRuntimeDomain && !t.wasiDom.open {
		t.wasiDom = nil
		if gp := getg(); gp == gp.m.curg {
			t.wasiDom = gp.wasiDom
		}
	}
	d := wasiDomainOf(t.wasiDom)
	if d == nil {
		return &pp.timers
	}
	return &d.timers
}

// wasiCheckTimers runs the due timers of every domain against the domain's own
// clock, and returns when the next of them is due (0 when none is).
//
//go:yeswritebarrierrec
func wasiCheckTimers() (pollUntil int64) {
	for i := 0; i < len(wasiDomains); i++ {
		d := wasiDomains[i]
		_, w, _ := d.timers.check(d.clock(), nil)
		if w != 0 && (pollUntil == 0 || w < pollUntil) {
			pollUntil = w
		}
	}
	return pollUntil
}

// wasiIdleDues records in each domain when its next timer is due, and returns
// the earliest of them. The runtime's own timers are left out: a host clock
// wait armed for runtime housekeeping would be a host call the program did not
// make, and would keep an otherwise idle component from suspending.
func wasiIdleDues() (next int64) {
	for _, d := range wasiDomains {
		d.idleDue = 0
		ts := &d.timers
		lock(&ts.mu)
		for _, tw := range ts.heap {
			t := tw.timer
			if t.astate.Load()&timerZombie != 0 || t.when <= 0 {
				continue
			}
			if d.idleDue == 0 || t.when < d.idleDue {
				d.idleDue = t.when
			}
		}
		unlock(&ts.mu)
		if d.idleDue != 0 && (next == 0 || d.idleDue < next) {
			next = d.idleDue
		}
	}
	return next
}

// wasiIdleWait reports, to an onIdle callback, the i-th open domain, the host
// context to arm its clock wait in, and when its next timer is due (0 when
// none is). ok is false past the last domain.
//
//go:linkname wasiIdleWait
func wasiIdleWait(i int) (dom, ctx unsafe.Pointer, due int64, ok bool) {
	if i >= len(wasiDomains) {
		return nil, nil, 0, false
	}
	d := wasiDomains[i]
	return unsafe.Pointer(d), d.ctx, d.idleDue, true
}

// wasiAdvanceTime records that a host clock wait armed for the domain has
// reached t.
//
//go:linkname wasiAdvanceTime
func wasiAdvanceTime(dom unsafe.Pointer, t int64) {
	if t > wasiLastNanotime {
		wasiLastNanotime = t
	}
	if d := (*wasiDomain)(dom); t > d.now {
		d.now = t
	}
}

// wasiLastNanotime is the latest monotonic reading that any goroutine made, or
// that a host clock wait reached. The runtime reads it instead of the host
// clock for its own work.
var wasiLastNanotime int64

func wasiSchedNow() int64 {
	if wasiLastNanotime == 0 {
		return 1
	}
	return wasiLastNanotime
}

func (d *wasiDomain) clock() int64 {
	if d.now == 0 {
		return 1
	}
	return d.now
}

// wasiObserveTime records a monotonic reading made by the program.
//
//go:nosplit
func wasiObserveTime(t int64) {
	if t > wasiLastNanotime {
		wasiLastNanotime = t
	}
	gp := getg()
	if gp == gp.m.curg {
		if d := wasiDomainOf(gp.wasiDom); d != nil && t > d.now {
			d.now = t
		}
	}
}

// wasiGoroutineNow is the clock of the calling goroutine's domain.
func wasiGoroutineNow() int64 {
	gp := getg()
	if gp == gp.m.curg {
		if d := wasiDomainOf(gp.wasiDom); d != nil {
			return d.clock()
		}
	}
	return wasiSchedNow()
}

// wasiTimerNow is the clock of t's domain.
func wasiTimerNow(t *timer) int64 {
	if d := wasiDomainOf(t.wasiDom); d != nil {
		return d.clock()
	}
	return wasiSchedNow()
}

// wasiGoschedClock reads the clock for a goroutine that yields while a timer of
// its domain is pending, so that the timer can fire.
func wasiGoschedClock() {
	gp := getg()
	if d := wasiDomainOf(gp.wasiDom); d != nil && d.timers.wakeTime() != 0 {
		nanotime1()
	}
}
