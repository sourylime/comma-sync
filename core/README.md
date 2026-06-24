# comma-sync core (Go) — WORK IN PROGRESS

A single cross-platform binary that will eventually back all the front-ends
(see [../ROADMAP.md](../ROADMAP.md), Phase 1). Built **alongside** the bash
script and macOS app — neither is affected, and both keep working as-is.

## Build & run

```bash
cd core
go build -o comma-sync .
ROOT="/path/to/Comma Footage" ./comma-sync list
./comma-sync discover
```

## Status — Phase 1 feature-complete

| Command | State | Notes |
|---------|-------|-------|
| `discover` | ✅ | native port scan + SSH probe for `/data/openpilot`, IP cache |
| `list [--json]` | ✅ | local chunks + device drives, ffprobe audio detection |
| `sync [--json]` | ✅ | download new drives (SFTP, resume) + stitch + ledger + cleanup |
| `restitch <route> [--json]` | ✅ | collision-safe; re-downloads if chunks absent |
| USB fallback (`USE_USB=1`) | ✅ | `adb forward` tunnel → `127.0.0.1` |
| `version` | ✅ | |

Connectivity is native `golang.org/x/crypto/ssh` + `github.com/pkg/sftp` — **no
`ssh`/`rsync` binaries required**. `ffmpeg`/`ffprobe` are still shelled out to.
Downloads preserve device mtimes (the recording-start stamp depends on them) and
resume partial files. `sync`/`restitch` emit one JSON object per line with `--json`.

### Verified
- `discover`, `list`/`--json` against the live device and locally.
- Full **stitch path** (concat + audio mux + collision-safe naming) and the
  **offline `sync` + ledger** flow, via synthetic fixtures.
- Cross-OS **build** (ubuntu/macOS/windows) in CI.

### Not yet exercised live
- The SFTP download from a device and USB tunnel were validated by code review +
  the proven bash equivalents, but not run end-to-end here (device was offline).
  First real run should confirm a device pull.

## What's next (Phase 2)
A cross-platform GUI (Tauri) that calls this binary and consumes its `--json`
progress stream. The bash `comma-sync.sh` remains the reference implementation.
