package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

var segRe = regexp.MustCompile(`^(.*)--(\d+)$`)

func segNum(s string) int {
	if m := segRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[2])
		return n
	}
	return 0
}

func stampFromEpoch(epoch int64) string {
	return time.Unix(epoch, 0).Format("2006-01-02_15-04-05")
}

// qcamHeadBytes is how much of a qcamera.ts is enough to see which streams it carries.
// A transport stream repeats its program table several times a second, so this covers it
// many times over while staying small enough to pull off the comma for every drive.
const qcamHeadBytes = 24 * 1024

// hasAudioFile reports whether a file carries an audio stream. For a transport stream the
// head of the file answers it outright, which keeps indexing quick; anything else — or a
// head too short to be sure — falls through to ffprobe.
func hasAudioFile(path string) bool {
	if strings.HasSuffix(path, ".ts") {
		if f, err := os.Open(path); err == nil {
			buf := make([]byte, qcamHeadBytes)
			n, _ := io.ReadFull(f, buf)
			f.Close()
			if audio, known := tsHasAudio(buf[:n]); known {
				return audio
			}
		}
	}
	return ffprobeHasAudio(path)
}

func ffprobeHasAudio(path string) bool {
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a",
		"-show_entries", "stream=codec_type", "-of", "csv=p=0", path).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "audio")
}

// listDrives merges drives whose chunks are on this computer with drives still on
// the comma (only the latter are "device").
func listDrives() []Drive {
	device := listDevice()
	devByRoute := map[string]Drive{}
	for _, d := range device {
		devByRoute[d.Route] = d
	}

	seen := map[string]bool{}
	var out []Drive
	for _, d := range listLocal() {
		seen[d.Route] = true
		// If the drive is still on the comma, trust the comma's recording-start stamp,
		// camera set and segment count. They describe the whole drive, so the index
		// stays correct even when the local copy is only partially downloaded — a partial
		// local copy can otherwise compute an earliest mtime off the wrong segment and
		// show a timestamp that won't match the finished output.
		if dev, ok := devByRoute[d.Route]; ok {
			d.Stamp = dev.Stamp
			if len(dev.Cameras) > len(d.Cameras) {
				d.Cameras = dev.Cameras
			}
			if dev.Segments > d.Segments {
				d.Segments = dev.Segments
			}
			// Chunks pulled without audio can't answer the audio question; the comma can.
			if d.HasAudio == nil {
				d.HasAudio = dev.HasAudio
			}
		}
		out = append(out, d)
	}
	for _, d := range device {
		if !seen[d.Route] {
			out = append(out, d)
		}
	}

	// Drives we only still have as stitched per-camera videos (raw chunks gone, no longer
	// on the comma). Surface them keyed by stamp so their folder isn't duplicated with a
	// local/device entry, and so you can build new derived outputs from the old videos.
	stampSeen := map[string]bool{}
	for _, d := range out {
		stampSeen[d.Stamp] = true
	}
	stitched := listStitched()
	// A drive that's been stitched but is still on the comma keeps its "device" row — the
	// comma is the authority on what the drive contains. But its videos ARE already in the
	// output folder, and without saying so the row looks like work still to do. Mark it,
	// using the scan we already have rather than re-probing the output folder per drive.
	doneStamps := map[string]bool{}
	for _, d := range stitched {
		doneStamps[d.Stamp] = true
	}
	for i := range out {
		if doneStamps[out[i].Stamp] {
			out[i].Synced = true
		}
	}
	for _, d := range stitched {
		if !stampSeen[d.Stamp] {
			d.Synced = true
			out = append(out, d)
			stampSeen[d.Stamp] = true
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Stamp > out[j].Stamp })
	return out
}

func listLocal() []Drive {
	dir := chunksDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	routeSegs := map[string][]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if m := segRe.FindStringSubmatch(e.Name()); m != nil {
			routeSegs[m[1]] = append(routeSegs[m[1]], e.Name())
		}
	}

	var drives []Drive
	for route, segs := range routeSegs {
		sort.Slice(segs, func(i, j int) bool { return segNum(segs[i]) < segNum(segs[j]) })
		var mtimes []int64
		var sizeKB int64
		camSet := map[string]bool{}
		lastQ := ""
		for _, seg := range segs {
			segPath := filepath.Join(dir, seg)
			files, _ := os.ReadDir(segPath)
			for _, f := range files {
				info, err := f.Info()
				if err != nil {
					continue
				}
				sizeKB += info.Size() / 1024
				name := f.Name()
				if strings.HasSuffix(name, ".hevc") {
					camSet[name] = true
					mtimes = append(mtimes, info.ModTime().Unix())
				}
				if name == "qcamera.ts" {
					lastQ = filepath.Join(segPath, name) // segs sorted asc -> last wins
				}
			}
		}
		var cams []string
		for c := range camSet {
			cams = append(cams, labelFor(c))
		}
		sort.Strings(cams)
		// Prefer the pinned stamp (recorded from the comma or at stitch time) so the
		// listed time can't drift with local file mtimes and matches the output folder.
		stamp := recordedStamp(route)
		if stamp == "" {
			stamp = route
			if e := earliestSaneMtime(mtimes); e > 0 {
				stamp = stampFromEpoch(e)
			}
		}
		// No qcamera.ts here means the chunks were pulled without audio, not that the
		// drive is silent — leave it unknown so the comma (or a later download) can say.
		var audio *bool
		if lastQ != "" {
			a := hasAudioFile(lastQ)
			audio = &a
		}
		drives = append(drives, Drive{
			Route: route, Stamp: stamp, Cameras: cams,
			HasAudio: audio, SizeKB: sizeKB, Segments: len(segs), Location: "local",
		})
	}
	return drives
}

// remoteListScript mirrors device_drives_remote() in comma-sync.sh.
const remoteListScript = `cd /data/media/0/realdata 2>/dev/null || exit 0
for r in $(ls -1d *--*/ 2>/dev/null | sed -E "s#--[0-9]+/##" | sort -u); do
  [ "$r" = "boot" ] && continue
  cnt=$(ls -1d ${r}--*/ 2>/dev/null | wc -l | tr -d " ")
  [ "$cnt" = "0" ] && continue
  mt=$(for f in ${r}--*/*.hevc; do [ -e "$f" ] && stat -c %Y "$f"; done | sort -n | awk '{a[NR]=$1} END{if(NR==0)exit; mx=a[NR]; for(i=1;i<=NR;i++) if(mx-a[i]<86400){print a[i]; exit}}')
  cams=$(ls ${r}--*/*.hevc 2>/dev/null | xargs -n1 basename 2>/dev/null | sort -u | tr "\n" "," | sed "s/,$//")
  sz=$(du -sk ${r}--* 2>/dev/null | awk "{s+=\$1} END{print s+0}")
  echo "${r}|${mt}|${cams}|${cnt}|${sz}"
done`

// remoteAudioScript sends back the head of one qcamera.ts per drive, base64'd so it
// survives the shell. It picks the LAST segment big enough to read: the newest segment
// of a drive that's still recording can be a stub, and judging a drive by a stub would
// report "no audio" on a drive that has it.
var remoteAudioScript = fmt.Sprintf(`cd /data/media/0/realdata 2>/dev/null || exit 0
for r in $(ls -1d *--*/ 2>/dev/null | sed -E "s#--[0-9]+/##" | sort -u); do
  [ "$r" = "boot" ] && continue
  q=""
  for f in ${r}--*/qcamera.ts; do
    [ -e "$f" ] || continue
    sz=$(stat -c %%s "$f" 2>/dev/null || echo 0)
    [ "$sz" -ge %d ] && q="$f"
  done
  [ -n "$q" ] || continue
  b=$(head -c %d "$q" | base64 | tr -d "\n")
  [ -n "$b" ] && echo "${r}|${b}"
done`, qcamHeadBytes, qcamHeadBytes)

// deviceAudio reports, per route, whether the comma recorded audio for it. Drives it
// can't judge are simply absent from the map — the index shows nothing rather than
// guessing "no audio", which is the one answer that would be actively misleading.
func deviceAudio(c *ssh.Client) map[string]bool {
	out, err := runCmd(c, remoteAudioScript)
	if err != nil {
		return nil
	}
	res := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		route, b64, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok || route == "" {
			continue
		}
		buf, derr := base64.StdEncoding.DecodeString(b64)
		if derr != nil {
			continue
		}
		if audio, known := tsHasAudio(buf); known {
			res[route] = audio
		}
	}
	return res
}

func listDevice() []Drive {
	host, port, cleanup, err := target()
	if err != nil {
		return nil
	}
	defer cleanup()
	c, err := dial(host, port, 8*time.Second)
	if err != nil {
		return nil
	}
	defer c.Close()
	return listDeviceWith(c)
}

func listDeviceWith(c *ssh.Client) []Drive {
	out, _ := runCmd(c, remoteListScript)
	audioByRoute := deviceAudio(c)

	var drives []Drive
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, "|")
		if len(f) < 5 {
			continue
		}
		route := f[0]
		stamp := route
		if mt, err := strconv.ParseInt(f[1], 10, 64); err == nil && mt > 0 {
			stamp = stampFromEpoch(mt)
			// Pin the device's authoritative stamp, repairing an earlier pin that was
			// poisoned by a pre-clock-sync first segment.
			pinDeviceStamp(route, stamp)
		}
		var cams []string
		for _, cam := range strings.Split(f[2], ",") {
			if cam != "" {
				cams = append(cams, labelFor(cam))
			}
		}
		sizeKB, _ := strconv.ParseInt(f[4], 10, 64)
		segs, _ := strconv.Atoi(f[3])
		var audio *bool
		if a, ok := audioByRoute[route]; ok {
			audio = &a
		}
		drives = append(drives, Drive{
			Route: route, Stamp: stamp, Cameras: cams,
			HasAudio: audio, SizeKB: sizeKB, Segments: segs, Location: "device",
		})
	}
	return drives
}
