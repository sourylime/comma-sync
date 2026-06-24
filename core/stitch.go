package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func localSegs(route string) []string {
	entries, _ := os.ReadDir(chunksDir())
	var segs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if m := segRe.FindStringSubmatch(e.Name()); m != nil && m[1] == route {
			segs = append(segs, e.Name())
		}
	}
	sort.Slice(segs, func(i, j int) bool { return segNum(segs[i]) < segNum(segs[j]) })
	return segs
}

func localRoutes() []string {
	entries, _ := os.ReadDir(chunksDir())
	set := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			if m := segRe.FindStringSubmatch(e.Name()); m != nil {
				set[m[1]] = true
			}
		}
	}
	var rs []string
	for r := range set {
		rs = append(rs, r)
	}
	sort.Strings(rs)
	return rs
}

func removeRouteChunks(route string) {
	for _, s := range localSegs(route) {
		_ = os.RemoveAll(filepath.Join(chunksDir(), s))
	}
}

// routeStamp = earliest hevc mtime across the route's segments (recording start).
func routeStamp(route string, segs []string) string {
	var earliest int64 = 1 << 62
	for _, s := range segs {
		files, _ := os.ReadDir(filepath.Join(chunksDir(), s))
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".hevc") {
				if info, err := f.Info(); err == nil {
					if mt := info.ModTime().Unix(); mt < earliest {
						earliest = mt
					}
				}
			}
		}
	}
	if earliest < (1 << 62) {
		return stampFromEpoch(earliest)
	}
	return route
}

func camerasOf(segs []string) []string {
	set := map[string]bool{}
	for _, s := range segs {
		files, _ := os.ReadDir(filepath.Join(chunksDir(), s))
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".hevc") {
				set[f.Name()] = true
			}
		}
	}
	var cams []string
	for c := range set {
		cams = append(cams, c)
	}
	sort.Strings(cams)
	return cams
}

// collisionSuffix finds the lowest " (N)" (or "") so no camera output is overwritten.
func collisionSuffix(outdir, stamp string, cams []string) string {
	exists := func(sfx string) bool {
		for _, cam := range cams {
			p := filepath.Join(outdir, stamp+"__"+labelFor(cam)+sfx+".mp4")
			if _, err := os.Stat(p); err == nil {
				return true
			}
		}
		return false
	}
	if !exists("") {
		return ""
	}
	for n := 2; ; n++ {
		sfx := fmt.Sprintf(" (%d)", n)
		if !exists(sfx) {
			return sfx
		}
	}
}

func concatFiles(dir string, segs []string, fname, pattern string) (string, bool, error) {
	tmp, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", false, err
	}
	any := false
	for _, s := range segs {
		p := filepath.Join(dir, s, fname)
		if f, err := os.Open(p); err == nil {
			_, _ = io.Copy(tmp, f)
			f.Close()
			any = true
		}
	}
	tmp.Close()
	return tmp.Name(), any, nil
}

func muxToMP4(combinedHEVC, audioPath, out string) error {
	args := []string{"-y", "-loglevel", "error", "-framerate", fps(), "-i", combinedHEVC}
	if audioPath != "" {
		args = append(args, "-i", audioPath, "-map", "0:v:0", "-map", "1:a:0",
			"-c:v", "copy", "-c:a", "copy", "-tag:v", "hvc1", out)
	} else {
		args = append(args, "-c", "copy", "-tag:v", "hvc1", out)
	}
	return exec.Command("ffmpeg", args...).Run()
}

// stitchRoute mirrors stitch_route() in comma-sync.sh: concat each camera's HEVC,
// mux microphone audio from qcamera.ts when present, collision-safe when asked.
func stitchRoute(route string, collision bool) error {
	dir := chunksDir()
	segs := localSegs(route)
	if len(segs) == 0 {
		return fmt.Errorf("no local chunks for %s", route)
	}
	cams := camerasOf(segs)
	if len(cams) == 0 {
		return fmt.Errorf("no camera files for %s", route)
	}
	stamp := routeStamp(route, segs)
	outdir := filepath.Join(rootDir(), stamp)
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		return err
	}
	suffix := ""
	if collision {
		suffix = collisionSuffix(outdir, stamp, cams)
	}
	emit(ProgressEvent{Type: "drive", Route: route, Message: fmt.Sprintf("Stitching drive %s  ->  %s%s", route, stamp, suffix)})

	audioPath := ""
	if withAudio() {
		ts, any, err := concatFiles(dir, segs, "qcamera.ts", "comma_aud_*.ts")
		if err == nil && any && hasAudioFile(ts) {
			audioPath = ts
			defer os.Remove(audioPath)
		} else {
			os.Remove(ts)
		}
	}

	ok := true
	for _, cam := range cams {
		lbl := labelFor(cam)
		out := filepath.Join(outdir, stamp+"__"+lbl+suffix+".mp4")
		combined, _, err := concatFiles(dir, segs, cam, "comma_*.hevc")
		if err != nil {
			emit(ProgressEvent{Type: "error", Route: route, Message: "concat failed for " + lbl})
			ok = false
			continue
		}
		emit(ProgressEvent{Type: "progress", Route: route, Phase: "stitch", Percent: 0})
		if err := muxToMP4(combined, audioPath, out); err != nil {
			emit(ProgressEvent{Type: "error", Route: route, Message: "ffmpeg failed for " + lbl})
			ok = false
		} else {
			tag := ""
			if audioPath != "" {
				tag = " +audio"
			}
			logf("      %s%s: %s", lbl, tag, filepath.Base(out))
		}
		os.Remove(combined)
	}
	if !ok {
		return fmt.Errorf("stitch failed for %s", route)
	}
	return nil
}
