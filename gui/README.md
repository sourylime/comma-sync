# Comma Sync GUI (Tauri) — WORK IN PROGRESS

A cross-platform desktop front-end (Linux + Windows; could replace the macOS
SwiftUI app too) that drives the [`core`](../core) `comma-sync` binary and renders
its `--json` progress stream. See [../ROADMAP.md](../ROADMAP.md), Phase 2.

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
via `$COMMA_SYNC_BIN`, then next to the app, then `PATH`.

## Prerequisites

- Rust (`https://rustup.rs`) and the Tauri v2 system deps for your OS
  (see https://tauri.app/start/prerequisites/ — on Linux: `webkit2gtk-4.1`, GTK,
  librsvg, patchelf, appindicator).
- The `comma-sync` **core** binary built (`cd ../core && go build -o comma-sync .`)
  and on `PATH` or pointed to via `COMMA_SYNC_BIN`.
- `ffmpeg`/`ffprobe` available (the core shells out to them).

## Build & run (dev)

```bash
cargo install tauri-cli --version '^2'      # once
cd gui/src-tauri
COMMA_SYNC_BIN=../../core/comma-sync cargo tauri dev
```

Release bundles (`.app`/`.dmg`, `.deb`/`.AppImage`, `.msi`):

```bash
cargo tauri build
```

## Status

| Area | State |
|------|-------|
| Main window (folders, toggles, Sync/Stop/Index) | ✅ drafted |
| Indexing Results sheet (list, select, Download All/Selected) | ✅ drafted |
| Per-drive live progress from `--json` stream | ✅ drafted |
| Compiles on Linux/macOS/Windows | ⏳ to verify in CI |
| Core as a bundled **sidecar** (vs. PATH lookup) | 🔜 todo |
| Full release bundling + icons (`tauri icon`) | 🔜 todo |

## TODO before merge

1. Verify `cargo build`/`cargo tauri build` on all three OSes (CI).
2. Bundle the core as a Tauri **sidecar** so users don't install it separately.
3. Generate the full icon set (`cargo tauri icon icon.png`).
4. Real end-to-end test against a comma on each OS.
5. Process-group kill on cancel (so a killed core also stops its `ffmpeg`).
