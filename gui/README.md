# Comma Sync GUI (Tauri) — WORK IN PROGRESS

A desktop front-end for **Linux and Windows** that drives the [`core`](../core)
`comma-sync` binary and renders its `--json` progress stream. See
[../ROADMAP.md](../ROADMAP.md), Phase 2.

> **macOS keeps its native SwiftUI app** ([`../macos-app`](../macos-app)) — it
> looks and feels nicer than a web view, so this Tauri GUI deliberately targets
> only Linux and Windows. The UI here mirrors the SwiftUI app's layout and style.

Built on a **separate branch** — `main` is untouched until this is tested on all
three OSes.

## Layout

```
gui/
  src/                  static web UI (no bundler — uses Tauri's global API)
    index.html  styles.css  app.js
  src-tauri/            Rust shell
    src/main.rs         commands: pick_folder, list_drives, start_job, cancel_job
    Cargo.toml  tauri.conf.json  build.rs  capabilities/  icons/
```

The Rust side spawns the `comma-sync` core and forwards its events to the UI
(`core-event` = JSON progress line, `core-stderr`, `core-done`). It finds the core
via `$COMMA_SYNC_BIN`, then next to the app, then `PATH`. In the packaged
installers the core ships **bundled as a Tauri sidecar** right next to the app, so
end users don't install it separately.

## Download & install (Linux / Windows)

Every push to this branch builds installers in CI. Grab them from the latest
**[Actions run](https://github.com/sourylime/comma-sync/actions)** → the
`comma-sync-gui-ubuntu-latest` / `comma-sync-gui-windows-latest` artifacts:

- **Linux:** `.AppImage` (chmod +x and run) or `.deb` (`sudo apt install ./*.deb`).
- **Windows:** `.msi` (or NSIS `*-setup.exe`).

**One runtime dependency:** `ffmpeg`/`ffprobe` must be installed (the core shells
out to them for stitching). Everything else — SSH/SFTP transfer, discovery — is
native in the core, no `rsync`/`ssh` needed.

- Linux: `sudo apt install ffmpeg` (Debian/Ubuntu) or `sudo dnf install ffmpeg`.
- Windows: `winget install Gyan.FFmpeg` (or `choco install ffmpeg`), then reopen
  your terminal so it's on `PATH`.

## Build from source (dev)

Prereqs: Rust (`https://rustup.rs`), Go, and the Tauri v2 system deps for your OS
(see https://tauri.app/start/prerequisites/ — on Linux: `webkit2gtk-4.1`, GTK,
librsvg, patchelf, appindicator), plus `ffmpeg`/`ffprobe` at runtime.

Because the core is wired in as a sidecar (`externalBin`), build it for your
machine **before** running/bundling the Tauri app:

```bash
cargo install tauri-cli --version '^2'      # once
triple="$(rustc -vV | sed -n 's/^host: //p')"
( cd core && go build -o "../gui/src-tauri/binaries/comma-sync-$triple" . )   # .exe on Windows
cd gui/src-tauri
cargo tauri dev          # run it
cargo tauri build        # or produce installers (.deb/.AppImage, .msi, .dmg)
```

## Status

| Area | State |
|------|-------|
| Main window (folders, toggles, Sync/Stop/Index) | ✅ drafted |
| Indexing Results sheet (list, select, Download All/Selected) | ✅ drafted |
| Per-drive live progress from `--json` stream | ✅ drafted |
| Core bundled as a Tauri **sidecar** | ✅ done |
| CI builds Linux `.deb`/`.AppImage` + Windows `.msi` installers | ✅ done |
| Real end-to-end test against a comma (Linux + Windows) | ⏳ in progress |

## TODO before merge

1. Install the CI artifacts on a real Linux + Windows machine and verify the UI
   launches, finds the comma, indexes, and stitches end-to-end.
2. Process-group kill on cancel (so a killed core also stops its `ffmpeg`).
3. Generate the full icon set (`cargo tauri icon icon.png`) for nicer platform icons.
