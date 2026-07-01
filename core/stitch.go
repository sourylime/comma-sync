package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// mp4OK reports whether an MP4 is finished and playable. An interrupted render (e.g.
// the Mac slept mid-stitch) leaves a file with no moov atom and no readable duration,
// so we only trust/reuse a video that ffprobe reports a positive duration for.
func mp4OK(path string) bool {
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		return false
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration",
		"-of", "default=nk=1:nw=1", path).Output()
	if err != nil {
		return false
	}
	d, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return d > 0
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
// Renders to a ".part" temp and renames into place only once ffmpeg succeeds AND the
// result is verifiably playable, so an interrupted/failed render never leaves a broken
// file at the final path (later runs would otherwise mistake it for a finished video).
func muxCamera(combinedHEVC, audioPCM string, audioDur float64, out string) error {
	part := out + ".part"
	os.Remove(part)
	var cmd *exec.Cmd
	if audioPCM == "" {
		cmd = exec.Command("ffmpeg", "-y", "-loglevel", "error",
			"-framerate", fps(), "-i", combinedHEVC, "-c", "copy", "-tag:v", "hvc1", "-f", "mp4", part)
	} else {
		vdur := float64(countPackets(combinedHEVC)) / fpsFloat()
		tempo := 1.0
		if vdur > 0 {
			if tempo = audioDur / vdur; tempo < 0.5 {
				tempo = 0.5
			} else if tempo > 2.0 {
				tempo = 2.0
			}
		}
		cmd = exec.Command("ffmpeg", "-y", "-loglevel", "error",
			"-framerate", fps(), "-i", combinedHEVC,
			"-f", "s16le", "-ar", strconv.Itoa(audioRate), "-ac", "1", "-i", audioPCM,
			"-map", "0:v:0", "-map", "1:a:0", "-c:v", "copy",
			"-filter:a", fmt.Sprintf("atempo=%.6f", tempo),
			"-c:a", "aac", "-b:a", "96k", "-tag:v", "hvc1", "-f", "mp4", part)
	}
	if err := cmd.Run(); err != nil {
		os.Remove(part)
		return err
	}
	if !mp4OK(part) {
		os.Remove(part)
		return fmt.Errorf("render incomplete (not playable)")
	}
	return os.Rename(part, out)
}

// combinedLayoutTag reads the "csync-layout=<roles>" signature we embed in a combined
// MP4's comment tag (e.g. "road,driver,wide" = primary,bottom-left,bottom-right), or ""
// if the file has no such tag (older files, or not one of ours).
func combinedLayoutTag(path string) string {
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format_tags=comment",
		"-of", "default=nk=1:nw=1", path).Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	if strings.HasPrefix(s, "csync-layout=") {
		return strings.TrimPrefix(s, "csync-layout=")
	}
	return ""
}

// freeCombinedPath returns the lowest-numbered combined output path in outdir that
// doesn't exist yet ("__combined.mp4", then "__combined (2).mp4", ...), so a new layout
// never overwrites an already-rendered combined with a different layout.
func freeCombinedPath(outdir, stamp string) string {
	base := filepath.Join(outdir, stamp+"__combined")
	if _, err := os.Stat(base + ".mp4"); os.IsNotExist(err) {
		return base + ".mp4"
	}
	for n := 2; ; n++ {
		p := fmt.Sprintf("%s (%d).mp4", base, n)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p
		}
	}
}

// combineVideo builds one optional multi-angle MP4 from the per-camera MP4s in outdir:
// 2 cams side by side at native aspect; 3 -> primary across the top 2/3, secondary
// bottom-left, tertiary bottom-right. Roles from PRIMARY/SECONDARY/TERTIARY_CAM (labels
// road|wide|driver); missing/duplicate roles are skipped, fewer than 2 does nothing.
//
// It is layout-aware: the chosen layout is stamped into the output's metadata, and before
// rendering it scans EVERY existing combined export in the folder. If one already has this
// exact layout it skips (re-encode not needed); otherwise it renders a NEW export to a free
// name so a different layout never clobbers an existing one. Re-encodes (HW on macOS).
func combineVideo(outdir, stamp, suffix string) {
	seen := map[string]bool{}
	var inputs, roles []string
	for _, r := range []string{primaryCam(), secondaryCam(), tertiaryCam()} {
		if seen[r] {
			continue
		}
		p := filepath.Join(outdir, stamp+"__"+r+suffix+".mp4")
		if mp4OK(p) {
			inputs = append(inputs, p)
			roles = append(roles, r)
			seen[r] = true
		}
	}
	if len(inputs) < 2 {
		return
	}
	layout := strings.Join(roles, ",") // e.g. "road,driver,wide" (primary,bottom-left,bottom-right)

	// Already rendered with this exact layout? Skip and say so. (Checks every combined
	// export in the folder, including "__combined (2).mp4" variants.)
	existing, _ := filepath.Glob(filepath.Join(outdir, stamp+"__combined*.mp4"))
	for _, m := range existing {
		if combinedLayoutTag(m) == layout {
			logf("      combined [%s] already rendered — skipped re-encode: %s", layout, filepath.Base(m))
			return
		}
	}

	var fc string
	if len(inputs) == 2 {
		fc = "[0:v]scale=-2:1208[a];[1:v]scale=-2:1208[b];[a][b]hstack=inputs=2[v]"
	} else {
		fc = "[0:v]scale=1920:1200:force_original_aspect_ratio=decrease,pad=1920:1200:(ow-iw)/2:(oh-ih)/2[p];" +
			"[1:v]scale=960:600:force_original_aspect_ratio=decrease,pad=960:600:(ow-iw)/2:(oh-ih)/2[s];" +
			"[2:v]scale=960:600:force_original_aspect_ratio=decrease,pad=960:600:(ow-iw)/2:(oh-ih)/2[t];" +
			"color=c=black:s=1920x1800:r=" + fps() + "[bg];[bg][p]overlay=0:0[b1];[b1][s]overlay=0:1200[b2];[b2][t]overlay=960:1200:shortest=1[v]"
	}

	out := freeCombinedPath(outdir, stamp)
	part := out + ".part"
	os.Remove(part)
	args := []string{"-y", "-loglevel", "error"}
	for _, p := range inputs {
		// On macOS, hardware-decode the HEVC inputs on the VideoToolbox media engine
		// (we already HW-encode there) — ~24% faster and ~1/3 the CPU on a 3-cam
		// combine, identical output, with automatic software fallback. Linux/Windows
		// stay on the software path (HW decode there is GPU-vendor-specific).
		if runtime.GOOS == "darwin" {
			args = append(args, "-hwaccel", "videotoolbox")
		}
		args = append(args, "-i", p)
	}
	args = append(args, "-filter_complex", fc, "-map", "[v]", "-map", "0:a?")
	if runtime.GOOS == "darwin" {
		args = append(args, "-c:v", "h264_videotoolbox", "-b:v", "14M")
	} else {
		args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", "22", "-pix_fmt", "yuv420p")
	}
	// Stamp the layout so a later run can tell what's in this file without re-parsing it.
	args = append(args, "-c:a", "copy", "-movflags", "+faststart",
		"-metadata", "comment=csync-layout="+layout, "-f", "mp4", part)
	if err := exec.Command("ffmpeg", args...).Run(); err != nil || !mp4OK(part) {
		os.Remove(part)
		emit(ProgressEvent{Type: "error", Message: "combined video failed for " + stamp})
		return
	}
	os.Rename(part, out)
	logf("      combined (%d cams, %s): %s", len(inputs), layout, filepath.Base(out))
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

	// If the per-camera videos are already in the output folder, reuse them: don't
	// re-stitch the individuals, just let combineVideo decide about the combined. It
	// renders one only if a combined with the CURRENT layout isn't already present, so
	// switching the primary/secondary/tertiary and re-running produces the new layout
	// (as a new export) instead of silently doing nothing or overwriting the old one.
	if withCombined() {
		allExist := true
		for _, cam := range cams {
			if !mp4OK(filepath.Join(outdir, stamp+"__"+labelFor(cam)+".mp4")) {
				allExist = false
				break
			}
		}
		if allExist {
			logf("==> %s: individual videos already exist — checking combined layout", stamp)
			combineVideo(outdir, stamp, "")
			return nil
		}
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
	if ok && withCombined() {
		combineVideo(outdir, stamp, suffix)
	}
	if !ok {
		return fmt.Errorf("stitch failed for %s", route)
	}
	return nil
}
