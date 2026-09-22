# Golem's Go fork

This is a fork of [golang/go](https://github.com/golang/go) that
[Golem](https://github.com/golemcloud/golem)'s Go SDK uses to compile agents
into WebAssembly components. It carries two patches:

1. **`runtime.wasiOnIdle` for wasip1**, from
   [dicej/go](https://github.com/dicej/go), the fork
   [componentize-go](https://github.com/bytecodealliance/componentize-go)
   downloads by default. It lets a WASI host run its event loop when the Go
   scheduler is idle, without which an async (WASI-p3) component deadlocks.
   Upstream: [golang/go#76775](https://github.com/golang/go/pull/76775), open.
2. **Goroutine scheduling-latency sampling disabled on wasip1.** The sampler
   reads the clock on every eighth transition of a goroutine, counted over that
   goroutine's whole life — a host call whose placement depends on the entire
   execution history. Golem records and replays host calls, and legitimately
   resumes a guest without re-executing its history (snapshot recovery), so
   those reads cannot be reproduced and replay diverges. The sampler feeds two
   metrics that carry no information on a single-threaded wasm guest, and no
   runtime decision depends on them.

Releases (tags `go<version>-golem.<n>`) publish bootstrap toolchain tarballs in
the layout componentize-go expects; the Golem CLI downloads the one for your
platform. See **[GOLEM.md](GOLEM.md)** for the patch rationale, the branch and
tag scheme, how to cut a release, and how to rebase onto a new Go version.

Everything below is upstream Go's own README.

---

# The Go Programming Language

Go is an open source programming language that makes it easy to build simple,
reliable, and efficient software.

![Gopher image](https://golang.org/doc/gopher/fiveyears.jpg)
*Gopher image by [Renee French][rf], licensed under [Creative Commons 4.0 Attribution license][cc4-by].*

Our canonical Git repository is located at https://go.googlesource.com/go.
There is a mirror of the repository at https://github.com/golang/go.

Unless otherwise noted, the Go source files are distributed under the
BSD-style license found in the LICENSE file.

### Download and Install

#### Binary Distributions

Official binary distributions are available at https://go.dev/dl/.

After downloading a binary release, visit https://go.dev/doc/install
for installation instructions.

#### Install From Source

If a binary distribution is not available for your combination of
operating system and architecture, visit
https://go.dev/doc/install/source
for source installation instructions.

### Contributing

Go is the work of thousands of contributors. We appreciate your help!

To contribute, please read the contribution guidelines at https://go.dev/doc/contribute.

Note that the Go project uses the issue tracker for bug reports and
proposals only. See https://go.dev/wiki/Questions for a list of
places to ask questions about the Go language.

[rf]: https://reneefrench.blogspot.com/
[cc4-by]: https://creativecommons.org/licenses/by/4.0/
