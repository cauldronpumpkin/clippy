# Clippy Engineering Setup

`README.md` is the quick-start guide for running Clippy on macOS and Windows.
This file is the engineering note for the repo's current state, development
requirements, and the remaining implementation work.

## Current State

This repository is no longer just a collection of reusable packages. It now
contains a runnable CLI and daemon entrypoint at `cmd/clippy/main.go` that
wires together:

- `internal/osclip` for native clipboard watch/apply
- `internal/netmesh` for local network discovery and WebSocket transport
- `internal/clipwire` for payload serialization and chunking
- `internal/app` for runtime orchestration, detached launch, and local control

Today Clippy can:

- watch the local clipboard for text, URLs, and images
- write inbound clipboard payloads into the local clipboard
- discover peers on the local network and exchange clipboard frames
- expose local daemon control commands through `start`, `status`, `stop`, and
  the internal `daemon` subcommand

Clippy is runnable from source today, but it is not yet a polished consumer app.

## What Is Still Left To Implement

The main product gaps are now above the transport/runtime layer:

- packaged macOS `.app` distribution
- packaged Windows installer or release-distributed `.exe` flow
- a user-friendly install path for non-developers
- persistent settings/config UX beyond flags and environment variables
- startup/login integration and background service management
- polished logging, diagnostics, and supportability guidance for end users
- documentation and test hardening for macOS environments polluted by external
  `CGO_LDFLAGS` or unrelated dynamic library settings

What is not present in this repo today:

- GUI
- tray/menu bar app
- installer
- auto-start at login
- release packaging pipeline

## Developer Setup

### Requirements

- Go 1.25.x
- macOS 12+ or Windows 10+
- local network where mDNS peer discovery is allowed

Additional macOS requirement:

- Xcode Command Line Tools

Why macOS is special:

- `golang.design/x/clipboard` uses Cocoa on macOS
- CGO must be enabled for clipboard integration

### Verify The Environment

macOS:

```bash
go version
xcode-select -p
go env CGO_ENABLED
```

Windows PowerShell:

```powershell
go version
go env CGO_ENABLED
```

Expected:

- `go version` reports Go 1.25.x or newer compatible toolchain
- macOS `xcode-select -p` prints a valid developer tools path
- `go env CGO_ENABLED` prints `1` on macOS

## Build And Run From Source

macOS:

```bash
go build -o clippy ./cmd/clippy
./clippy start --foreground
```

Windows PowerShell:

```powershell
go build -o clippy.exe ./cmd/clippy
.\clippy.exe start --foreground
```

In another terminal:

macOS:

```bash
./clippy status
./clippy stop
```

Windows PowerShell:

```powershell
.\clippy.exe status
.\clippy.exe stop
```

Notes:

- `start` runs detached by default; `--foreground` is the easiest way to
  validate a new setup
- `daemon` exists for the detached launcher path and is not the primary
  end-user command
- the default control endpoint is `<os.TempDir()>/clippy.sock` on Unix-like
  systems
- the default control endpoint is `\\.\pipe\clippy-<USERNAME>` on Windows

## Test Status And Known Caveat

The lightweight package tests pass, but this macOS environment exposes a real
linker/runtime caveat for clipboard-backed binaries and tests:

- `go build ./cmd/clippy` succeeds here, but prints linker warnings about
  duplicate `-lobjectbox`
- the resulting `clippy` binary also aborts at startup in that polluted
  environment because it tries to load `@rpath/libobjectbox.dylib`
- `go test ./...` fails in `internal/app` and `internal/osclip` because the
  produced test binaries try to load `@rpath/libobjectbox.dylib`

This is not a Clippy dependency declared by the repo. It is coming from the
local shell or linker environment.

If you hit similar failures on macOS, inspect and temporarily clear custom CGO
linker flags before rebuilding or rerunning tests:

```bash
echo "${CGO_LDFLAGS:-}"
unset CGO_LDFLAGS
go build -o clippy ./cmd/clippy
go test ./...
```

If the environment still injects unrelated libraries, also inspect other
toolchain variables such as `CGO_CPPFLAGS`, `CGO_CFLAGS`, and `LDFLAGS`.

In this environment, rebuilding after unsetting `CGO_LDFLAGS` was sufficient to
produce a working CLI binary that passed the `status`, `start --foreground`,
and `stop` smoke checks.

## Recommended Next Engineering Work

If the goal is to turn Clippy into a consumer-ready app, the next implementation
work should focus on packaging and operations instead of the clipboard/mesh
core:

1. Add a releaseable build/distribution path for macOS and Windows.
2. Define persistent config storage and a user-facing settings surface.
3. Add startup-at-login or service integration on both platforms.
4. Improve operational logging and troubleshooting documentation.
