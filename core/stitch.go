package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const audioRate = 16000 // comma mic: 16 kHz mono

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

// countPackets returns the video frame count of an HEVC/MP4 file (cheap; no decode).
func countPackets(path string) int {
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v",
		"-count_packets", "-show_entries", "stream=nb_read_packets", "-of", "csv=p=0", path).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

func fpsFloat() float64 {
	if f, err := strconv.ParseFloat(fps(), 64); err == nil && f > 0 {
		return f
	}
	return 20
}

// segFirstHEVC returns a camera .hevc filename present in a segment (any camera —
// they share the same frame count), or "" if the segment has none.
func segFirstHEVC(segPath string) string {
	files, _ := os.ReadDir(segPath)
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".hevc") {
			return f.Name()
		}
	}
	return ""
}

// buildAudioPCM builds a 16 kHz mono PCM track locked to the video. The comma's mic
// starts a few seconds AFTER the camera at the start of a drive (the first qcamera.ts
// has a leading gap), so each segment's audio is decoded *preserving its timing*
// (aresample fills the gap with silence) and fit to that segment's exact video
// length, so audio and video realign at every segment boundary instead of drifting.
// Segments with no audio get silence. Returns the PCM path ("" if no audio) + its dur.
func buildAudioPCM(dir string, segs []string) (string, float64) {
	out, err := os.CreateTemp("", "comma_aud_*.pcm")
	if err != nil {
		return "", 0
	}
	hadAudio := false
	for _, s := range segs {
		segPath := filepath.Join(dir, s)
		hevc := segFirstHEVC(segPath)
		if hevc == "" {
			continue
		}
		frames := countPackets(filepath.Join(segPath, hevc))
		if frames <= 0 {
			continue
		}
		segDur := float64(frames) / fpsFloat()
		tbytes := int64(segDur*float64(audioRate)+0.5) * 2
		var wrote int64
		qts := filepath.Join(segPath, "qcamera.ts")
		if _, err := os.Stat(qts); err == nil {
			tmp, _ := os.CreateTemp("", "comma_seg_*.pcm")
			tmpName := tmp.Name()
			tmp.Close()
			cmd := exec.Command("ffmpeg", "-y", "-v", "error", "-i", qts,
				"-map", "0:a:0", "-af", "aresample=async=1:first_pts=0,apad",
				"-t", fmt.Sprintf("%.4f", segDur), "-ar", strconv.Itoa(audioRate),
				"-ac", "1", "-f", "s16le", tmpName)
			if cmd.Run() == nil {
				if data, err := os.ReadFile(tmpName); err == nil && len(data) > 0 {
					hadAudio = true
					if int64(len(data)) > tbytes {
						data = data[:tbytes]
					}
					out.Write(data)
					wrote = int64(len(data))
				}
			}
			os.Remove(tmpName)
		}
		if wrote < tbytes {
			out.Write(make([]byte, tbytes-wrote)) // pad with silence to keep sync
		}
	}
	out.Close()
	if !hadAudio {
		os.Remove(out.Name())
		return "", 0
	}
	st, _ := os.Stat(out.Name())
	return out.Name(), float64(st.Size()/2) / float64(audioRate)
}

// muxCamera writes one camera's MP4. With audio it stretches the PCM to exactly fill
// this camera's video (atempo) so the totals line up, and re-encodes to AAC (copying
// the concatenated-TS audio fails with "sample rate not set").
func muxCamera(combinedHEVC, audioPCM string, audioDur float64, out string) error {
	if audioPCM == "" {
		return exec.Command("ffmpeg", "-y", "-loglevel", "error",
			"-framerate", fps(), "-i", combinedHEVC, "-c", "copy", "-tag:v", "hvc1", out).Run()
	}
	vdur := float64(countPackets(combinedHEVC)) / fpsFloat()
	tempo := 1.0
	if vdur > 0 {
		if tempo = audioDur / vdur; tempo < 0.5 {
			tempo = 0.5
		} else if tempo > 2.0 {
			tempo = 2.0
		}
	}
	return exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-framerate", fps(), "-i", combinedHEVC,
		"-f", "s16le", "-ar", strconv.Itoa(audioRate), "-ac", "1", "-i", audioPCM,
		"-map", "0:v:0", "-map", "1:a:0", "-c:v", "copy",
		"-filter:a", fmt.Sprintf("atempo=%.6f", tempo),
		"-c:a", "aac", "-b:a", "96k", "-tag:v", "hvc1", out).Run()
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

	audioPCM, audioDur := "", 0.0
	if withAudio() {
		if audioPCM, audioDur = buildAudioPCM(dir, segs); audioPCM != "" {
			defer os.Remove(audioPCM)
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
		if err := muxCamera(combined, audioPCM, audioDur, out); err != nil {
			emit(ProgressEvent{Type: "error", Route: route, Message: "ffmpeg failed for " + lbl})
			ok = false
		} else {
			tag := ""
			if audioPCM != "" {
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
