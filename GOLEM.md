# Golem's Go fork

This is [golang/go](https://github.com/golang/go) with two patches that
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
`src/bootstrap.bash`, checks that both patches are present in the built tree,
and uploads the tarballs and their `.sha256` files to the release. Go toolchain
builds are reproducible, so anyone can rebuild a tag and compare checksums.

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
git cherry-pick <wasiOnIdle commits> <sampling commit>   # from golem-go1.27
```

Take the `wasiOnIdle` commits from dicej's branch for that Go version when it
exists — it is the upstream of that patch — and keep authorship. Then open the
cherry-picks as pull requests, tag `go1.28.0-golem.1`, and run the release
script. Check whether either patch has been obsoleted upstream first: if
golang/go#76775 lands, patch 1 is no longer needed; if Go gains a knob for
scheduler sampling, patch 2 becomes a GODEBUG setting in the Golem CLI instead.
