package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const audioRate = 16000 // comma mic: 16 kHz mono

// audioRenderVer identifies how a per-camera video's audio track was built. Bump it
// whenever that changes, so videos made by an older version are RE-STITCHED instead of
// being reused. Without this, re-stitching a drive whose per-camera videos already
// exist keeps their old audio forever — the segment count is unchanged, so the reuse
// check sees nothing wrong — and an audio fix silently never reaches finished drives.
// "6" = mic warm-up gap only on the first segment; later shortfalls pad at the end.
const audioRenderVer = "6"

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
	// A pinned stamp (from the comma's listing, or an earlier stitch) always wins, so
	// output folders and the index agree and never drift with local file mtimes.
	if s := recordedStamp(route); s != "" {
		return s
	}
	var mtimes []int64
	for _, s := range segs {
		files, _ := os.ReadDir(filepath.Join(chunksDir(), s))
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".hevc") {
				if info, err := f.Info(); err == nil {
					mtimes = append(mtimes, info.ModTime().Unix())
				}
			}
		}
	}
	// Skip a pre-clock-sync first segment so the folder isn't dated months early.
	if earliest := earliestSaneMtime(mtimes); earliest > 0 {
		s := stampFromEpoch(earliest)
		recordStamp(route, s) // pin it so every later list/stitch reuses this exact time
		return s
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

// angleSpread returns how far apart the per-camera videos are in length, plus the
// longest and shortest labels. Equal length is what keeps every angle showing the same
// instant; a gap means the composites will drift.
func angleSpread(outdir, stamp, suffix string, cams []string) (spread float64, longest, shortest string) {
	type ad struct {
		lbl string
		dur float64
	}
	var ds []ad
	for _, c := range cams {
		p := filepath.Join(outdir, stamp+"__"+labelFor(c)+suffix+".mp4")
		if v := mp4Duration(p); v > 0 {
			ds = append(ds, ad{labelFor(c), v})
		}
	}
	if len(ds) < 2 {
		return 0, "", ""
	}
	lo, hi := ds[0], ds[0]
	for _, x := range ds {
		if x.dur < lo.dur {
			lo = x
		}
		if x.dur > hi.dur {
			hi = x
		}
	}
	return hi.dur - lo.dur, hi.lbl, lo.lbl
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

// micGapSecs works out how far into a segment the microphone actually started, given
// how much audio the segment really contains (audioDur) versus how long its video is.
//
// The container's stream start_time is useless here — qcamera.ts reports the same
// start for both streams even when the microphone came alive ten seconds later — so
// this works from the first real PACKET of each stream, cross-checked against how much
// audio the segment actually holds.
// maxMidDriveGap caps the head gap allowed anywhere except the very first segment.
// The microphone warms up once, at the start of a recording; by the second segment it
// is already running and cannot begin seconds late. A large apparent head gap there
// means the segment's audio is short at the END (a truncated write or transfer), and
// putting that shortfall at the front instead shoves the sound seconds late for the
// rest of the drive.
const maxMidDriveGap = 0.5

// It returns its reasoning as text rather than logging it, because segments are measured
// concurrently and lines written from several at once would interleave into nonsense.
// The caller prints them back in segment order.
func micGapSecs(qts, chunk string, segDur, audioDur float64, firstSegment bool) (float64, string) {
	// The microphone can be late at the START of a segment and can also stop slightly
	// early at the END. Only the HEAD gap may be re-inserted as leading silence; a tail
	// loss must be padded at the end instead. Lumping both at the front (which the plain
	// "how much is missing" figure does) pushes the sound late by the tail amount.
	head := 0.0
	fv, okv := firstPacketPTS(qts, "v")
	fa, oka := firstPacketPTS(qts, "a")
	if okv && oka {
		head = fa - fv
	}
	shortfall := segDur - audioDur
	if shortfall <= 0.05 && head <= 0.05 {
		return 0, "" // audio covers the segment and starts with it
	}
	if !firstSegment && head > maxMidDriveGap {
		// Mid-drive: the mic was already running, so this shortfall belongs at the end.
		return 0, fmt.Sprintf("      segment audio is %.2fs short — padding the END (the mic was already running)", shortfall)
	}

	// Prefer the packet-timed head gap: it is the direct measurement, and it separates
	// a late start from an early finish. Accept it only when it is physically
	// consistent — the audio placed there has to fit inside the segment.
	if head > 0.05 && head+audioDur <= segDur+0.5 {
		// Those timestamps only describe qcamera.ts internally. Anchor them to the
		// camera chunk the video is really built from, so the sound cannot sit ahead of
		// the picture by however far qcamera's own video stream started late.
		if chunk != "" {
			if d := qcamChunkOffset(qts, chunk); d != 0 &&
				head+d > 0 && head+d+audioDur <= segDur+0.5 {
				return head + d, fmt.Sprintf("      qcamera runs %+.2fs vs the camera chunk — mic gap corrected to %.2fs", d, head+d)
			}
		}
		return head, ""
	}

	// Packet timing unusable — some containers report the audio starting with the video
	// even when it plainly does not. Fall back to the amount of audio that is missing,
	// which can only be attributed to a late start — and only the first segment can
	// have one.
	if !firstSegment || shortfall <= 0.05 || shortfall >= segDur {
		return 0, ""
	}
	return shortfall, ""
}

// firstPacketPTS returns the presentation time of the first packet of a stream
// ("v" or "a"), which is when that stream's data actually begins.
func firstPacketPTS(path, stream string) (float64, bool) {
	out, err := exec.Command("ffprobe", "-v", "error",
		"-select_streams", stream, "-show_entries", "packet=pts_time",
		"-of", "csv=p=0", "-read_intervals", "%+#1", path).Output()
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(out))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	v, err := strconv.ParseFloat(strings.Trim(s, " ,"), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// segFirstHEVC returns a camera .hevc filename present in a segment (any camera —
// they share the same frame count), or "" if the segment has none.
func segFirstHEVC(segPath string) string {
	// Prefer the ROAD camera: the microphone is recorded alongside it (qcamera.ts is the
	// road view), so the road defines the audio's timeline. Reading the directory order
	// instead picked dcamera.hevc — the driver — and any per-segment frame difference
	// between the driver and the camera being muxed accumulated segment after segment
	// into audible drift.
	for _, cam := range []string{"fcamera.hevc", "ecamera.hevc", "dcamera.hevc"} {
		if fi, err := os.Stat(filepath.Join(segPath, cam)); err == nil && fi.Size() > 0 {
			return cam
		}
	}
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
// segAudio is one segment's decoded audio and the timing measured for it. The measuring
// is what takes the time — an ffmpeg decode plus a couple of ffprobe calls per segment —
// so it's done for all segments concurrently and only the assembly stays sequential.
type segAudio struct {
	pcm    string  // temp file of decoded audio ("" = this segment has none)
	gap    float64 // silence to insert at the front (late microphone)
	tbytes int64   // exact size of this segment's slot in the finished track
	note   string  // what the gap measurement concluded, printed later in order
	slot   bool    // false = no known video length, so this segment occupies nothing
}

func buildAudioPCM(dir string, segs []string, ref map[string]int) (string, float64) {
	out, err := os.CreateTemp("", "comma_aud_*.pcm")
	if err != nil {
		return "", 0
	}

	res := make([]segAudio, len(segs))
	// Each segment is independent, and the work is mostly waiting on ffmpeg/ffprobe.
	// Done one at a time this was a long silent stretch on a half-hour drive.
	workers := runtime.NumCPU()
	if workers > 4 { // each worker spawns ffmpeg; more than this just thrashes the disk
		workers = 4
	}
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	next := make(chan int)
	var done int64
	go func() {
		for i := range segs {
			next <- i
		}
		close(next)
	}()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				res[i] = measureSegAudio(dir, segs[i], ref)
				n := atomic.AddInt64(&done, 1)
				emit(ProgressEvent{Type: "progress", Route: curRoute, Phase: "render",
					Percent: float64(n) / float64(len(segs)) * 100, Message: "audio track"})
			}
		}()
	}
	wg.Wait()

	// Assemble strictly in segment order, so the finished track is identical to what the
	// sequential version produced and the log reads in order.
	hadAudio := false
	var gapSecs []segGapInfo
	for i, s := range segs {
		r := res[i]
		if r.note != "" {
			logf("%s", r.note)
		}
		if !r.slot {
			continue
		}
		var wrote int64
		if r.pcm != "" {
			if data, err := os.ReadFile(r.pcm); err == nil && len(data) > 0 {
				hadAudio = true
				if r.gap > 0 {
					gapSecs = append(gapSecs, segGapInfo{seg: s, secs: r.gap})
					// Hold the content back by the measured gap, then trim/pad so the
					// segment occupies exactly its slot and the next starts on time.
					gapBytes := int64(r.gap*float64(audioRate)+0.5) * 2
					if gapBytes > r.tbytes {
						gapBytes = r.tbytes
					}
					out.Write(make([]byte, gapBytes))
					wrote += gapBytes
				}
				if int64(len(data)) > r.tbytes-wrote {
					data = data[:r.tbytes-wrote]
				}
				out.Write(data)
				wrote += int64(len(data))
			}
			os.Remove(r.pcm)
		}
		if wrote < r.tbytes {
			out.Write(make([]byte, r.tbytes-wrote)) // pad with silence to keep sync
		}
	}
	out.Close()
	if !hadAudio {
		os.Remove(out.Name())
		return "", 0
	}
	reportMicGaps(gapSecs)
	st, _ := os.Stat(out.Name())
	return out.Name(), float64(st.Size()/2) / float64(audioRate)
}

// measureSegAudio decodes one segment's audio and works out where it belongs. It touches
// nothing shared, so segments can be measured in parallel.
func measureSegAudio(dir, s string, ref map[string]int) segAudio {
	segPath := filepath.Join(dir, s)
	// Use the SAME per-segment length the VIDEO was built to. Where a camera's chunk
	// was missing, the video got frozen frames to fill the gap — so measuring one
	// camera's real frames here would make the audio shorter than the picture, and
	// everything after that gap would slide progressively out of sync.
	frames := 0
	if r, ok := ref[s]; ok && r > 0 {
		frames = r
	} else if hevc := segFirstHEVC(segPath); hevc != "" {
		frames = countPackets(filepath.Join(segPath, hevc))
	}
	if frames <= 0 {
		return segAudio{}
	}
	segDur := float64(frames) / fpsFloat()
	r := segAudio{slot: true, tbytes: int64(segDur*float64(audioRate)+0.5) * 2}

	qts := filepath.Join(segPath, "qcamera.ts")
	if fi, err := os.Stat(qts); err != nil || fi.Size() == 0 {
		return r
	}
	// Decode the segment's audio in full FIRST, so we know exactly how much of it
	// exists. That amount is what tells us when the microphone started — see
	// micGapSecs. Trimming during the decode (the old approach) would destroy the
	// very measurement we need.
	tmp, err := os.CreateTemp("", "comma_seg_*.pcm")
	if err != nil {
		return r
	}
	tmpName := tmp.Name()
	tmp.Close()
	cmd := exec.Command("ffmpeg", "-y", "-v", "error", "-i", qts,
		"-map", "0:a:0", "-af", "aresample=async=1:first_pts=0",
		"-ar", strconv.Itoa(audioRate), "-ac", "1", "-f", "s16le", tmpName)
	if cmd.Run() != nil {
		os.Remove(tmpName)
		return r
	}
	fi, err := os.Stat(tmpName)
	if err != nil || fi.Size() == 0 {
		os.Remove(tmpName)
		return r
	}
	audioDur := float64(fi.Size()/2) / float64(audioRate)
	r.gap, r.note = micGapSecs(qts, filepath.Join(segPath, segFirstHEVC(segPath)),
		segDur, audioDur, segNum(s) == 0)
	r.pcm = tmpName
	return r
}

// segGapInfo records that a segment's microphone started late.
type segGapInfo struct {
	seg  string
	secs float64
}

// reportMicGaps says where the microphone was late, so a drive whose sound looks off at
// the start can be explained from the log instead of guessed at.
func reportMicGaps(g []segGapInfo) {
	if len(g) == 0 {
		return
	}
	var parts []string
	total := 0.0
	for _, x := range g {
		total += x.secs
		parts = append(parts, fmt.Sprintf("minute %d: %.1fs", segNum(x.seg), x.secs))
	}
	logf("      microphone started late in %d segment(s) — %s (silence inserted so the sound stays with the picture)",
		len(g), strings.Join(parts, ", "))
	if total > 0 {
		logf("         total silence added: %.1fs", total)
	}
}

// muxCamera writes one camera's MP4. With audio it stretches the PCM to exactly fill
// this camera's video (atempo) so the totals line up, and re-encodes to AAC (copying
// the concatenated-TS audio fails with "sample rate not set").
// Renders to a ".part" temp and renames into place only once ffmpeg succeeds AND the
// result is verifiably playable, so an interrupted/failed render never leaves a broken
// file at the final path (later runs would otherwise mistake it for a finished video).
func muxCamera(combinedHEVC, audioPCM string, audioDur float64, out string, segCount int, label string) error {
	part := out + ".part"
	os.Remove(part)
	// Stamp how many segments this was stitched from, so a later reuse check can tell
	// whether the drive has since had more downloaded (a stale/partial output).
	segTag := "comment=csync-segs=" + strconv.Itoa(segCount) + ";csync-audio=" + audioRenderVer
	// Total length so the muxing shows a live percentage. Even though the video is a
	// stream copy (fast), a big drive still takes real time to write, and without a bar
	// this first pass looked frozen to the user.
	nframes := countPackets(combinedHEVC)
	vsecs := float64(nframes) / fpsFloat()
	var args []string
	if audioPCM == "" {
		args = []string{"-y", "-loglevel", "error",
			"-framerate", fps(), "-i", combinedHEVC, "-c", "copy", "-tag:v", "hvc1",
			"-metadata", segTag, "-f", "mp4", part}
	} else {
		// NO time-stretching. The PCM is built segment by segment in lockstep with the
		// video, so it is already aligned all the way through. Stretching it to make the
		// totals match (the old atempo) spreads any small total difference across the
		// WHOLE drive as a rate error — sound that starts in sync and slides further
		// behind the longer you watch. Instead pad with silence / cut at the very END,
		// where a small residual is inaudible and cannot accumulate.
		if d := audioDur - vsecs; d > 1.0 || d < -1.0 {
			logf("      note: audio track is %+.1fs vs this camera's video — padded/trimmed at the end (never stretched)", d)
		}
		// Match the PCM to this camera's exact video length by cutting or adding silence
		// at the END. Done here in Go rather than with an ffmpeg filter so the result is
		// exact and predictable.
		pcm, cleanup := fitPCM(audioPCM, int64(vsecs*float64(audioRate)+0.5)*2)
		if cleanup != nil {
			defer cleanup()
		}
		args = []string{"-y", "-loglevel", "error",
			"-framerate", fps(), "-i", combinedHEVC,
			"-f", "s16le", "-ar", strconv.Itoa(audioRate), "-ac", "1", "-i", pcm,
			"-map", "0:v:0", "-map", "1:a:0", "-c:v", "copy",
			"-c:a", "aac", "-b:a", "96k", "-tag:v", "hvc1",
			"-metadata", segTag, "-f", "mp4", part}
	}
	if err := runFFmpegProgress(args, vsecs, nframes, label); err != nil {
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
	v, _ := mp4CommentTag(path, "csync-layout=")
	return v
}

// freeVariantPath returns the lowest-numbered output path of the given kind in outdir
// that doesn't exist yet ("__<kind>.mp4", then "__<kind> (2).mp4", ...), so a new
// variant (a different combined layout, a different vertical arrangement) never
// overwrites an already-rendered file of the same kind.
func freeVariantPath(outdir, stamp, kind string) string {
	base := filepath.Join(outdir, stamp+"__"+kind)
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

// combinedRoles returns the camera roles that make up the combined video for this
// drive, in order. Shared by the renderer and the completeness check so the two can
// never disagree about which layout should exist. "none" (offered for the tertiary
// slot) leaves that slot empty on purpose.
func combinedRoles(outdir, stamp, suffix string) []string {
	seen := map[string]bool{}
	var roles []string
	for _, r := range []string{primaryCam(), secondaryCam(), tertiaryCam()} {
		if r == "" || r == "none" || seen[r] {
			continue
		}
		if mp4OK(filepath.Join(outdir, stamp+"__"+r+suffix+".mp4")) {
			roles = append(roles, r)
			seen[r] = true
		}
	}
	return roles
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
	roles := combinedRoles(outdir, stamp, suffix)
	if len(roles) < 2 {
		return
	}
	var inputs []string
	for _, r := range roles {
		inputs = append(inputs, filepath.Join(outdir, stamp+"__"+r+suffix+".mp4"))
	}
	layout := strings.Join(roles, ",") // e.g. "road,driver,wide" (primary,bottom-left,bottom-right)

	// Already rendered with this exact layout AND still up to date? Skip. (Checks every
	// combined export in the folder, including "__combined (2).mp4" variants.) If a
	// same-layout combined exists but is older than the per-camera videos (they were
	// re-stitched), rebuild it in place rather than leaving it stale.
	out := ""
	existing, _ := filepath.Glob(filepath.Join(outdir, stamp+"__combined*.mp4"))
	for _, m := range existing {
		if combinedLayoutTag(m) == layout {
			if outputFresh(m, inputs) {
				logf("      combined [%s] already rendered — skipped re-encode: %s", layout, filepath.Base(m))
				return
			}
			out = m // stale same-layout combined — rebuild it
			break
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

	if out == "" {
		out = freeVariantPath(outdir, stamp, "combined")
	}
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
	if err := runFFmpegProgress(args, mp4Duration(inputs[0]), 0, "combined video"); err != nil || !mp4OK(part) {
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
	curRoute = route // so the extra-output renderers tag progress with this drive
	segs := localSegs(route)
	if len(segs) == 0 {
		// No raw chunks — but if this drive's stitched per-camera videos are still in the
		// output folder, we can rebuild the derived outputs (combined/360/vertical) from
		// them. This is what lets you make new output types from old drives whose chunks
		// are gone and are no longer on the comma. Here `route` is the stamp folder name.
		if rebuildExtrasFromStitched(route) {
			return nil
		}
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

	// Work out where any camera's chunks are missing or short. Those stretches get
	// frozen frames rather than being skipped, so every angle stays the same length and
	// the composites never show the two views at different moments.
	tf := stepTimer("checking chunk lengths")
	fillRef, fillHave, gaps := planFills(segs, cams)
	tf()
	reportGaps(gaps)

	// If the per-camera videos are already in the output folder, reuse them: don't
	// re-stitch the individuals, just let combineVideo decide about the combined. It
	// renders one only if a combined with the CURRENT layout isn't already present, so
	// switching the primary/secondary/tertiary and re-running produces the new layout
	// (as a new export) instead of silently doing nothing or overwriting the old one.
	// Say which extra outputs are enabled for this drive, so the log always shows
	// exactly what was requested (and nothing appears without being asked for).
	onOff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	logf("      extra outputs: combined=%s · 360=%s · vertical=%s",
		onOff(withCombined()), onOff(with360()), onOff(withVertical()))

	if withCombined() || with360() || withVertical() {
		allExist := true
		for _, cam := range cams {
			p := filepath.Join(outdir, stamp+"__"+labelFor(cam)+".mp4")
			// Reuse only if the existing video is valid AND was stitched from the same
			// number of segments we have now — otherwise it's stale/partial (e.g. more
			// chunks were downloaded since) and must be re-stitched.
			if !mp4OK(p) || individualSegs(p) != len(segs) {
				allExist = false
				break
			}
			// Built by an older audio path — re-stitch so the fix actually lands.
			if withAudio() && !individualAudioCurrent(p) {
				logf("      %s was stitched by an older audio version — re-stitching it so the sound lines up", filepath.Base(p))
				allExist = false
				break
			}
		}
		// Refuse to reuse angles that are already out of step with each other — that's
		// what makes a composite show the same moment twice. Re-stitching them is the
		// only way to fix it, so fall through and rebuild.
		if allExist {
			if spread, long, short := angleSpread(outdir, stamp, "", cams); spread > 0.5 {
				logf("      existing angles are %.1fs apart (%s vs %s) — re-stitching them so the composites line up",
					spread, long, short)
				allExist = false
			}
		}
		if allExist {
			logf("==> %s: individual videos already exist — checking extra outputs", stamp)
			if withCombined() {
				combineVideo(outdir, stamp, "")
			}
			if with360() {
				equirect360Video(outdir, stamp, "")
			}
			if withVertical() {
				verticalVideo(outdir, stamp, "")
			}
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
		ta := stepTimer("building the audio track")
		if audioPCM, audioDur = buildAudioPCM(dir, segs, fillRef); audioPCM != "" {
			defer os.Remove(audioPCM)
		}
		ta()
	}

	tv := stepTimer("rendering the individual camera videos")
	ok := true
	for i, cam := range cams {
		lbl := labelFor(cam)
		out := filepath.Join(outdir, stamp+"__"+lbl+suffix+".mp4")
		combined, err := concatCameraFilled(dir, segs, cam, fillRef, fillHave)
		if err != nil {
			emit(ProgressEvent{Type: "error", Route: route, Message: "concat failed for " + lbl + ": " + err.Error()})
			ok = false
			continue
		}
		label := fmt.Sprintf("%s video (%d/%d)", lbl, i+1, len(cams))
		if err := muxCamera(combined, audioPCM, audioDur, out, len(segs), label); err != nil {
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
	tv()
	if ok {
		if spread, long, short := angleSpread(outdir, stamp, suffix, cams); spread > 0.5 {
			logf("      !! angles came out %.1fs apart (%s longer than %s) — composites may drift",
				spread, long, short)
		}
	}
	if ok && withCombined() {
		t := stepTimer("the combined video")
		combineVideo(outdir, stamp, suffix)
		t()
	}
	if ok && with360() {
		t := stepTimer("the 360 video")
		equirect360Video(outdir, stamp, suffix)
		t()
	}
	if ok && withVertical() {
		t := stepTimer("the vertical video")
		verticalVideo(outdir, stamp, suffix)
		t()
	}
	if !ok {
		return fmt.Errorf("stitch failed for %s", route)
	}
	return nil
}
