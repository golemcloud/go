# Golem's Go fork

This is [golang/go](https://github.com/golang/go) with six patches that
[Golem](https://github.com/golemcloud/golem)'s Go SDK needs in the toolchain
that compiles agents to WebAssembly components. The Golem CLI downloads a
release of this fork and builds Go components with it; nothing else about Go
changes, and the fork carries no Golem code.

## The patches

### 1. `runtime.wasiOnIdle` (from dicej/go)

Lets a WASI host run its own event loop whenever the Go scheduler has nothing
runnable. [componentize-go](https://github.com/bytecodealliance/componentize-go)
requires it to build components that use async/WASI-p3 features: without it an
async component deadlocks as soon as every goroutine waits on a host call.

Written by Joel Dice and maintained at
[dicej/go](https://github.com/dicej/go) (branch/tag `go<version>-wasi-on-idle`),
which is what componentize-go downloads by default. Upstream proposal:
[golang/go#76775](https://github.com/golang/go/pull/76775), still open.

### 2. Goroutine scheduling-latency sampling disabled on wasip1

`casgstatus` samples goroutine latencies on every `gTrackingPeriod`-th
transition out of `_Grunning`, keyed on a per-goroutine counter seeded randomly
at spawn and accumulated over the goroutine's whole life. Each sampled
transition calls `nanotime()` — a host call on wasip1.

Golem is a durable-execution host: it records the guest's host calls and
replays them positionally after a crash. It also resumes a guest *without*
re-executing its whole history — snapshot-based recovery skips the snapshotted
invocations, and snapshot save/load hooks run outside the recording. A counter
that sums the instance's lifetime transitions can therefore never be reproduced
on those paths, so the sampler's clock reads land in different places than the
recording has them and replay diverges.

Nothing in the runtime reads the tracking fields; they feed only
`/sched/latencies:seconds` and `/sync/mutex/wait/total:seconds`, which carry no
information on a single-threaded wasm guest. The patch gates the two places that
arm tracking on `const goroutineTracking = GOOS != "wasip1"`. A GODEBUG knob
would be the upstream-friendly form and is on Golem's list to propose.

### 3. GC timestamps without a wall-clock read on wasip1 (ours)

GC mark termination stamps `MemStats.LastGC` and `PauseEnd` with the wall clock.
A component-model host lowers large results into guest memory through
`cabi_realloc`, whose allocation can finish a GC cycle, and calling any import
from `cabi_realloc` is forbidden. The guest pauses the monotonic clock around
that allocation, so the wasip1 adapter serves monotonic reads from a cache, but
the adapter does not pause the wall clock: the GC's wall-clock read reaches the
host and traps the component ("cannot leave component instance").

The patch makes `time.Now` record the offset between the two clocks it has just
read (`lastWallMinusMono`), and mark termination adds that offset to the
monotonic time instead of reading the wall clock
(`const gcWallFromMono = GOOS == "wasip1"`). It issues no clock read of its own.
The proper fix is for the adapter to pause the wall clock too; once it does, this
patch can go.

### 4. The next timer's due time for the idle callback on wasip1 (ours)

`wasiOnIdle` hands control to the host whenever nothing is runnable, but the
callback is not told when the next Go timer is due. A timer is not something the
host can wait on, so an async component whose goroutines wait only on a timer —
`time.Sleep`, `time.After`, a context deadline — either exits without producing
its result or stays suspended until some unrelated host call completes.

The patch records, at each idle transition, when the next timer of each runtime
domain (patch 6) is due, and exposes it as `runtime.wasiIdleWait(i)`: the i-th
domain, the host context to wait in, and the due time. The callback (Golem's
fork of [go.bytecodealliance.org/pkg](https://github.com/golemcloud/go-pkg))
arms one host `wait-until` per domain that resumes the component when the timer
is due, and reports the completed wait with `runtime.wasiAdvanceTime`, which
advances that domain's clock so the scheduler fires the timer. It is additive: a
callback that ignores it behaves as before. It belongs upstream together with
patch 1.

Two details keep an idle component suspendable by a durable host. No clock is
read at the idle transition or by the callback: the wait is until an absolute
due time. And the runtime's own timers, such as the background scavenger's
sleep, are not reported: they are housekeeping that can wait until the component
runs again, and a clock wait armed for them would keep the component busy
forever (`go1.27.1-golem.3` had both problems: Golem agents awaiting a promise or
an RPC result never suspended).

### 5. No scavenging on wasip1 (ours)

The scavenger returns free heap pages to the operating system. On wasip1 it
cannot: a linear memory never shrinks, and `sysUnused` does nothing. A scavenge
still reads the monotonic clock to account for its work, and on wasip1 each read
is a host call. Two places start one: `mheap.allocSpan` scavenges inline when a
heap growth crosses the retained-memory goal or the memory limit, and the
background scavenger runs after a GC and then sleeps on a timer, which makes the
scheduler read the clock on every pass while the timer is pending.

Whether an allocation crosses the goal depends on the heap's whole history.
Golem's snapshot recovery restores the agent into a fresh instance, and the
snapshot save and load run outside the recording, so the heap after a restore
differs from the heap the original run had. The inline scavenge then fires in a
replayed invocation where the original did not, or the reverse, and replay
diverges on the clock read (`mallocgc` → `mheap.allocSpan` → `nanotime`).

The patch makes `gcPaceScavenger` leave both scavenger goals unset on wasip1 and
gates both inline branches of `allocSpan`, the memory-limit one and the
retained-memory one, on `const scavengeReleasesMemory = GOOS != "wasip1"`. The
gate is needed as well as the goals: until the first GC cycle paces the
scavenger its retained-memory goal is zero, so every heap growth scavenges, and
a restored instance starts in that state. The background scavenger then finds
no work and parks without a timer, and no allocation scavenges inline.
`debug.FreeOSMemory` still scavenges, but only when the program calls it. Nothing
is lost: the memory was never released on wasip1.

Scavenging was still the only thing that lowered `mappedReady`, which the
memory-limit heap goal compares with the limit. Without it, after the heap
shrinks from a peak above the memory limit, the goal's overage term would stay
at that peak for good and the GC would run continuously: with a limit lowered by
`debug.SetMemoryLimit` or a `GOMEMLIMIT` the heap once passed, about fifteen
times as many cycles, and once the GC CPU limiter turns off mark assists, a
mark phase that cannot finish until the goroutine yields, so the heap grows by
everything allocated meanwhile. So on wasip1 `memoryLimitHeapGoal` leaves free
pages out of `mappedReady`, as the scavenger's accounting did once it had
released them: they are reusable without growing the linear memory, and no GC
can make the memory smaller. The stats still report them as free, not released,
and without a memory limit the goal is unchanged. The upstream-friendly form is
to make `sysUnused` a no-op signal the scavenger honours on every platform where
it is one (wasip1 and js).

### 6. Goroutine-local host context and runtime domains on wasip1 (ours)

A WASI host can keep execution context per component task, and every goroutine
of a Go program shares one task. Golem keeps the scope of a custom durable
operation (`durability.Run` in the Golem Go SDK) there, so without help every
goroutine that runs while one operation waits would act inside that operation.
The runtime cannot tell the host when it switches goroutines, so the patch lets
the program do it. Each goroutine carries an opaque value (`g.wasiCtx`), and the
program registers three hooks with `runtime.wasiSetContextHooks`:

- switch, on the system stack in `execute`, when the goroutine about to run has
  another value than the one installed last;
- create, at a `go` statement before the child exists, whose result becomes the
  child's value;
- exit, in `goexit1`.

The runtime's own goroutines (GC workers, the finalizer and cleanup goroutines)
get no value. A goroutine that a timer function starts (`time.AfterFunc`) gets
the value of the domain whose timer it is.

A durable host also skips a completed operation on replay and returns its
recorded result. Runtime state that every goroutine shares would carry the
skipped part's effects into the rest of the program: its clock reads would fire
other goroutines' timers, and its map and `select` draws would shift the random
stream after it. So the program can run a part of itself in its own domain
(`runtime.wasiDomainEnter`, `wasiDomainExit`). A domain has its own clock, which
only reads made in the domain and the domain's host clock waits advance, its
own timers heap, and its own random state for `runtime.rand` and `cheaprand`. A
new domain is seeded with draws from its parent's state, so the parent's stream
advances by the same amount whether or not the part runs.

On wasip1 the runtime also makes no host call for itself: the scheduler, the GC,
the sweeper and the mutex profiler read `wasiLastNanotime`, the latest monotonic
reading any goroutine made (`nanotime`), and only the program's own reads
(`programNanotime`: `time.Now`, `time.Sleep`, netpoll deadlines) reach the
host. `sync.Mutex` starvation uses the clock of the waiting goroutine's domain. `Gosched` reads the clock when the calling
goroutine's domain has a pending timer, so a goroutine that polls timers that
way still sees them fire. The runtime's own work under a runtime lock, inside an
allocation or on the system stack draws from the M's random state, not a
domain's. `startTheWorld` skips its non-blocking `netpoll` when no goroutine
waits on the poller, because on wasip1 that poll is a host call.

The cost of the switch hook is one pointer compare per goroutine switch. The
patch has no Golem code; every name is `wasi*` and every branch is gated on
`GOOS == "wasip1"`.

## Branches and tags

- `golem-go1.27` — the integration branch, based on the upstream `go1.27.1` tag,
  and the default branch: open pull requests against it. One branch per Go minor
  (`golem-go1.28`, …), created from that minor's upstream tag with the patches
  cherry-picked, so the history stays readable against upstream.
- `go<version>-golem.<n>` — release tags on an integration branch. `<version>` is
  the base Go release, `<n>` counts Golem releases of it.

Upstream tags (`go1.27.1`, …) are pushed here as-is for provenance.

## Cutting a release

```shell
git switch golem-go1.27 && git pull
git tag go1.27.1-golem.1 && git push origin go1.27.1-golem.1
golem/release.sh go1.27.1-golem.1          # --dry-run to build without uploading
```

The script cross-builds `go-<os>-<arch>-bootstrap.tbz` for linux/amd64,
linux/arm64, darwin/amd64, darwin/arm64 and windows/amd64 using Go's own
`src/bootstrap.bash`, checks that all six patches are present in the built tree,
and uploads the tarballs and their `.sha256` files to the release. The checksums
identify the published artifacts; they are not a rebuild guarantee, because
`bootstrap.bash` packs the tree with its current file timestamps, so two builds
of the same tag produce tarballs that differ in metadata.

It runs on Linux or macOS (any host can cross-build every target) and needs a
host Go to bootstrap from (`GOROOT_BOOTSTRAP`, defaults to the `go` on PATH)
plus the GitHub CLI for the upload.

## How Golem consumes it

The Golem CLI pins a release tag, downloads the asset for the current platform
and extracts it into componentize-go's own toolchain directory
(`~/.cache/componentize-go/v2/go-<os>-<arch>-bootstrap`, `~/Library/Caches/…` on
macOS), then puts that `bin` first on `PATH` with `GOTOOLCHAIN=local` for every
Go command it runs. componentize-go then picks this toolchain instead of
downloading its own, and the build uses exactly the pinned version.

Set `GOLEM_GO_TOOLCHAIN=<go root>` to point the CLI at a locally built tree
instead — that is how you test a change to this fork before releasing it.

## Rebasing onto a new Go release

```shell
git fetch https://github.com/golang/go.git refs/tags/go1.28.0:refs/tags/go1.28.0
git switch -c golem-go1.28 go1.28.0
git cherry-pick <wasiOnIdle commits> <sampling commit> <gc wall-clock commit> <idle-timer commit> <scavenger commit> <context commit>   # from golem-go1.27
```

Take the `wasiOnIdle` commits from dicej's branch for that Go version when it
exists — it is the upstream of that patch — and keep authorship. Then open the
cherry-picks as pull requests, tag `go1.28.0-golem.1`, and run the release
script. Check whether any patch has been obsoleted upstream first: if
golang/go#76775 lands, patch 1 is no longer needed; if Go gains a knob for
scheduler sampling, patch 2 becomes a GODEBUG setting in the Golem CLI instead;
if the wasip1 adapter pauses the wall clock with the monotonic one, patch 3 is
no longer needed.
