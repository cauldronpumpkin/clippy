# Clippy

Clippy is a cross-device clipboard sync tool for the local network. It can
watch your clipboard, discover peers, and apply incoming clipboard content on
another machine.

There is no packaged installer or macOS app bundle in this repository yet.
Today, the practical quick-start path is to build the CLI from source and run
it directly.

## What You Get Today

- clipboard sync runtime with local peer discovery
- text, URL, and image clipboard support
- CLI commands to start, inspect, and stop the background daemon

What you do not get yet:

- macOS `.app`
- Windows installer
- tray app or GUI
- auto-start at login

## Quick Setup

### macOS

Requirements:

- Go 1.25.x
- Xcode Command Line Tools
- CGO enabled

Install and verify:

```bash
go version
xcode-select -p
go env CGO_ENABLED
```

Build:

```bash
go build -o clippy ./cmd/clippy
```

Run in the foreground the first time:

```bash
./clippy start --foreground
```

In another terminal, check status or stop it:

```bash
./clippy status
./clippy stop
```

If you want detached/background startup after verifying it works:

```bash
./clippy start
```

### Windows

Requirements:

- Windows 10 or later
- Go 1.25.x

Verify Go:

```powershell
go version
```

Build:

```powershell
go build -o clippy.exe ./cmd/clippy
```

Run in the foreground the first time:

```powershell
.\clippy.exe start --foreground
```

In another PowerShell window, check status or stop it:

```powershell
.\clippy.exe status
.\clippy.exe stop
```

If you want detached/background startup after verifying it works:

```powershell
.\clippy.exe start
```

## Commands

Clippy currently exposes these subcommands:

- `start` starts the daemon and detaches unless you pass `--foreground`
- `status` reports whether the daemon is running and lists discovered peers
- `stop` asks the running daemon to exit
- `daemon` runs the daemon process directly and is mainly used by the launcher

High-level options and environment variables:

- `--name` or `CLIPPY_NAME` sets the local node name
- `--listen-addr` and `--port` control the local listener
- `--service-name`, `--domain`, and `--websocket-path` adjust discovery and transport
- `--control` or `CLIPPY_CONTROL_ENDPOINT` overrides the local control socket or pipe

Default local control endpoints:

- macOS and other Unix-like systems: `<os.TempDir()>/clippy.sock`
- Windows: `\\.\pipe\clippy-<USERNAME>`

## Troubleshooting

### macOS build/test warnings about unrelated dylibs

In this repo, `go build ./cmd/clippy` can succeed while printing linker warnings
such as duplicate `-lobjectbox`. In the same polluted environment, the built
`clippy` binary and `go test ./...` can still fail at runtime because they try
to load an unrelated `libobjectbox.dylib`.

That dylib is not a Clippy dependency. It usually means your shell environment
is injecting custom linker flags.

Check and clear CGO linker flags, then rebuild:

```bash
echo "${CGO_LDFLAGS:-}"
unset CGO_LDFLAGS
go build -o clippy ./cmd/clippy
go test ./...
```

If the problem persists, inspect other environment variables such as
`CGO_CFLAGS`, `CGO_CPPFLAGS`, and `LDFLAGS`.

### Clipboard support notes

- macOS clipboard support depends on Cocoa through `golang.design/x/clipboard`
- Windows does not require the same CGO clipboard setup
- incoming image payloads are normalized to PNG before local write

## Current Status

Clippy is runnable from source and the clipboard/network runtime is wired, but
the project still needs packaging, installer UX, persistent settings, and
startup integration before it behaves like a finished desktop app.

For the engineering-oriented status and remaining implementation gaps, see
`SETUP.md`.
