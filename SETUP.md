# Clippy Setup

This repository is not a finished desktop app yet.

Right now it contains reusable Go packages:

- `internal/clipwire`: clipboard payload and chunking types
- `internal/netmesh`: local-network discovery and WebSocket transport
- `internal/osclip`: native clipboard read/write/watch integration

There is currently no `cmd/` folder, no `main.go`, and no packaged binary to double-click. So "setting up the app" currently means setting up the development environment so you can build, test, and integrate the modules.

## What Clippy Can Do Today

- Watch the local clipboard for text, URLs, and images
- Write incoming clipboard payloads into the local clipboard
- Discover peers on the local network and exchange binary frames

What is still missing:

- A runnable top-level app or CLI that wires these packages together
- Installer or packaged `.app` / `.exe`
- End-user settings, logging, and startup integration

## Fastest Path

If you just want to confirm your machine is ready:

1. Install Go from [go.dev/dl](https://go.dev/dl).
2. Open this repo in a terminal.
3. Run:

```bash
go mod download
go test ./...
```

If that passes, your machine is ready for Clippy development.

## macOS Setup

## Requirements

- macOS 12 or later is the current baseline on the official Go downloads page.
- Go installed from [go.dev/dl](https://go.dev/dl)
- Xcode Command Line Tools
- `CGO_ENABLED=1`

Why CGO matters:

- This repo uses [`golang.design/x/clipboard`](https://github.com/golang-design/clipboard).
- That library requires CGO on macOS and uses Cocoa for clipboard access.

## Step 1: Install Go

Install the macOS package from the official Go site:

- [Download and install Go](https://go.dev/doc/install)
- [Downloads page](https://go.dev/dl)

After installation, open a new Terminal window and verify:

```bash
go version
```

## Step 2: Install Xcode Command Line Tools

Run:

```bash
xcode-select --install
```

Then verify:

```bash
xcode-select -p
go env CGO_ENABLED
```

Expected:

- `xcode-select -p` prints a valid developer tools path
- `go env CGO_ENABLED` prints `1`

## Step 3: Open the Repo

```bash
cd /path/to/clippy
go mod download
```

## Step 4: Run the Test Suite

```bash
go test ./...
```

If you have custom linker flags in your shell environment, they can break CGO test binaries. A known example is `CGO_LDFLAGS`.

If `go test ./...` fails with a missing unrelated `.dylib`, retry with:

```bash
unset CGO_LDFLAGS
go test ./...
```

## Step 5: Confirm Clipboard Access Works

The clipboard package supports:

- UTF-8 text
- PNG images

A quick manual check for image clipboard behavior on macOS:

- Press `Ctrl+Shift+Cmd+4` to copy a screenshot to the clipboard

## Windows Setup

## Requirements

- Windows 10 or later
- Go installed from [go.dev/dl](https://go.dev/dl)

Why Windows is simpler:

- [`golang.design/x/clipboard`](https://github.com/golang-design/clipboard) does not require CGO on Windows
- No extra clipboard dependency is required

## Step 1: Install Go

Install the Windows MSI from:

- [Download and install Go](https://go.dev/doc/install)
- [Go on Windows](https://go.dev/wiki/Windows)

After installation, close and reopen PowerShell or Command Prompt, then verify:

```powershell
go version
```

## Step 2: Open the Repo

In PowerShell:

```powershell
cd C:\path\to\clippy
go mod download
```

## Step 3: Run the Test Suite

```powershell
go test ./...
```

## Step 4: Confirm Clipboard Access Works

The clipboard package supports:

- UTF-8 text
- PNG images

A quick manual check for image clipboard behavior on Windows:

- Press `Shift+Win+S` to copy a screenshot to the clipboard

## Current Limits You Should Know

- The repo does not yet contain a runnable `main` package.
- There is no packaged macOS app bundle.
- There is no packaged Windows `.exe` or installer.
- The clipboard layer always reads native images as PNG.
- Incoming JPEG clipboard payloads are normalized to PNG before writing locally.

## How To "Use" The Repo Right Now

Today, you use Clippy as a developer library, not as a consumer app.

Typical workflow:

1. Set up Go.
2. Run `go test ./...`.
3. Add a `cmd/clippy` entrypoint that wires together:
   - `internal/osclip.Manager`
   - `internal/netmesh.Manager`
   - `internal/clipwire` payload serialization

At a high level, the future app flow looks like this:

1. Start `osclip.Manager`
2. Listen on `osclip.Events()`
3. Convert events into network frames using `clipwire`
4. Broadcast frames with `netmesh`
5. On inbound network payloads, call `osclip.ApplyRemote(...)`

## Recommended Next Step

If your goal is a usable app rather than just a working dev environment, the next missing piece is a `cmd/clippy/main.go` that:

- starts the clipboard manager
- starts the network manager
- converts clipboard events into frames
- applies inbound frames back into the local clipboard

## Sources

- [Go install docs](https://go.dev/doc/install)
- [Go downloads](https://go.dev/dl)
- [Go on Windows](https://go.dev/wiki/Windows)
- [golang.design/x/clipboard](https://github.com/golang-design/clipboard)
