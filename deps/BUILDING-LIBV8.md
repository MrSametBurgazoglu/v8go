# Building libv8.a from source

`deps/<os>_<arch>/libv8.a` is V8 plus Chromium's libc++, compiled into one
static archive that cgo links. It is **not in git** (`.gitignore`): normally
`scripts/fetch-libv8.go` downloads the upstream release for `deps/v8_version`.
Build it yourself when the release does not have what Gezgin needs. The first
time was **Temporal** (2026-10-06): upstream builds with
`v8_enable_temporal_support=false`, so `globalThis.Temporal` did not exist and
about 6,600 test262 pages failed in Gezgin's WPT run.

> A fetched release archive overwrites a source-built one, and it has no
> Temporal. After `fetch-libv8.go`, rebuild, or restore your own copy.

## One command

```bash
deps/rebuild-libv8.sh
```

The first build took 39 minutes on 3 compile jobs (1 h 52 min of CPU, peak
memory 3.1 GB) and left about 7 GB in the workspace: 6.4 GB of V8 checkout and
0.7 GB of build output. It does what the sections below explain, in a workspace outside the repo
(`~/.cache/v8build`, or set `WORK=`). It then installs the archive, keeps the
old one as `libv8.a.prev`, and runs the v8go tests that touch V8.

| knob | default | meaning |
| --- | --- | --- |
| `WORK` | `~/.cache/v8build` | workspace: depot_tools, the V8 clone, ninja outputs |
| `JOBS` | 3 | ninja `-j`. Each clang job peaks around 1 GB |
| `MEMORY` / `CPU` | `4G` / `400%` | the systemd scope's `MemoryMax` / `CPUQuota` |
| `SLICE` | `gezgin.slice` | the shared memory ceiling every Gezgin build runs inside |
| extra args | | passed to `build.py`, e.g. `--no-temporal`, `--debug` |

Re-running is incremental: the clone, the gclient sync and the compiled
objects are reused, so a second run after a failure takes minutes.

## What it needs

- **git, python3, clang/clang++, Go.** No Rust install: V8's `DEPS` downloads
  Chromium's prebuilt Rust toolchain (`third_party/rust-toolchain`, about
  270 MB) along with its clang. `temporal_rs` is compiled with it.
- **Network** to `chromium.googlesource.com` and `storage.googleapis.com`
  (gclient sync and its hooks).
- **Disk:** about 7 GB. The V8 checkout with its dependencies is 6.4 GB, and
  the build directory is 0.7 GB.
- **Memory:** ninja's default `-j` is cores + 2, which needs more than 4 GB
  and was OOM-killed under the cap. Use `JOBS=3` for 4 GB, and raise both
  together.

## What build.py does

1. `gclient sync` for V8 at the checked-out tag (`deps/v8_version`, currently
   14.7.173.21). `managed: False` means gclient uses the existing checkout,
   and the hooks fetch gn, ninja, clang, Rust, the wasm spec tests and the
   sysroot bits.
2. Local patches (`apply_local_patches`): CREL relocations are stripped from
   `build/config/compiler/BUILD.gn`, because the GNU ld that cgo uses cannot
   read them.
3. `gn gen` with the args in `gn_args`: monolithic, no sandbox, ICU built in,
   custom libc++ on Linux, and `v8_enable_temporal_support` from
   `--temporal`/`--no-temporal`.
4. `ninja v8_monolith` plus libc++/libc++abi, then one archive,
   `<os>_<arch>/libv8.a`, holding the monolith, libc++, libc++abi and **V8's
   Rust code**: the crate rlibs under `obj/` (temporal_capi, temporal_rs, the
   ICU4X crates, Chromium's allocator shims) and the Rust standard library the
   build compiled in `local_rustc_sysroot`. With Temporal on, the archive is
   about 280 MB instead of about 140 MB.

## Pitfalls already hit

- **Undefined `temporal_rs_*` symbols at the cgo link.** `v8_monolith` holds
  only V8's C++. Chromium links Rust as rlibs at the final link, and cgo's link
  never saw them. `build.py`'s `rust_rlibs()` now merges them into libv8.a. An
  rlib is an ar archive; its `lib.rmeta` member is not an object, and the
  linker never loads it.

- **`gn gen` fails with "python3_bin_reldir.txt not found. need to initialize
  depot_tools".** depot_tools' `gn` wrapper needs a bootstrap that
  `DEPOT_TOOLS_UPDATE=0` skips. `build.py` now falls back to V8's own
  `buildtools/linux64/gn` and `third_party/ninja/ninja`.
- **The `build_for_node` gclient var does not turn Temporal off.** Only the gn
  arg `build_with_node` does (`gni/v8.gni`). Leave it alone.
- **The build is longer than the usual 15-minute cap.** Run it once, in its
  own scope with its own `RuntimeMaxSec` (the script uses 8 h), never in a
  test gate.
- **Headers:** `deps/include` must come from the same V8 version as the
  archive. A rebuild at the same `v8_version` needs no header change, but a
  version bump does (`upgrade_v8.py`).

## Checking the result

```bash
CC=clang CXX=clang++ CGO_LDFLAGS=-L$PWD/deps/linux_x86_64 go test -p 1 -run 'TestTemporal|TestIntl' .
```

`TestTemporal` (`temporal_test.go`) fails on an archive built without
Temporal. In Gezgin, the test262 Temporal pages
(`third_party/test262/test/built-ins/Temporal`) are the end-to-end check. Go
caches cgo link results by input hash, so a swapped archive is picked up on
the next build without `-a`.

## Upgrading V8

`deps/upgrade_v8.py` moves `v8_version` and copies the new headers into
`deps/include`. Then run `deps/rebuild-libv8.sh`. The script refuses a
workspace clone at another tag; delete `$WORK/deps/v8` and it clones the new
one.
