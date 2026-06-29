package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// keepAwake holds an OS assertion that stops the machine from idle-sleeping for the
// lifetime of a sync/stitch. A long drive can take a while to stitch, and if the Mac
// idle-sleeps part way through, ffmpeg is killed and leaves half-written, unplayable
// MP4s (no moov atom). On macOS this runs `caffeinate -i -m -w <pid>`, which releases
// on its own when this process exits. Returns a stop func (safe to call always); it's
// a no-op on other platforms or if caffeinate isn't available.
func keepAwake() func() {
	if runtime.GOOS != "darwin" {
		return func() {}
	}
	path, err := exec.LookPath("caffeinate")
	if err != nil {
		return func() {}
	}
	cmd := exec.Command(path, "-i", "-m", "-w", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		return func() {}
	}
	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

// sweepStaleTemps removes leftover "comma_*" scratch from a previously interrupted run
// (a hard kill skips the inline cleanup and can strand a multi-GB temp). Only touches
// entries older than a minute so a concurrent run's fresh scratch is never removed.
func sweepStaleTemps() {
	dir := os.TempDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-time.Minute)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "comma_") {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}
