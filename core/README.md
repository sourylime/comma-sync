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

## Status

| Command | State | Notes |
|---------|-------|-------|
| `discover` | ✅ working | native port scan + SSH probe for `/data/openpilot`, IP cache |
| `list [--json]` | ✅ working | local chunks + device drives, ffprobe audio detection |
| `sync [routes…]` | ⏳ stub | prints "use ../comma-sync.sh" for now |
| `restitch <route>` | ⏳ stub | same |
| `version` | ✅ | |

Connectivity uses native `golang.org/x/crypto/ssh` (no `ssh`/`rsync` binaries
needed). `ffprobe`/`ffmpeg` are still shelled out to.

## Next steps (to finish Phase 1)

1. **`sync`** — implement the pull with `github.com/pkg/sftp` over the existing
   SSH client (resume via file offset/size), honoring the ledger
   (`.processed_routes`) and the `MIN_AGE_SECS` "still recording" rule.
2. **Stitch** — concatenate each camera's HEVC segments and mux audio from
   `qcamera.ts` via `ffmpeg` (same flags as the script: `-framerate 20 -c copy
   -tag:v hvc1`, no `-shortest`). Emit `ProgressEvent` JSON lines (see types.go).
3. **`restitch`** — collision-safe `" (N)"` naming; re-download if chunks absent.
4. **USB mode** — `USE_USB=1` via `adb forward`, then point at `127.0.0.1`.
5. **CI** — uncomment the `go-core` matrix job in `.github/workflows/ci.yml` to
   build for ubuntu/macos/windows on every push.

The bash `comma-sync.sh` is the reference implementation for all of the above.
