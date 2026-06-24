package main

import (
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

// hasAudioFile shells out to ffprobe to check for an audio stream.
func hasAudioFile(path string) bool {
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
	seen := map[string]bool{}
	var out []Drive
	for _, d := range listLocal() {
		seen[d.Route] = true
		out = append(out, d)
	}
	for _, d := range listDevice() {
		if !seen[d.Route] {
			out = append(out, d)
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
		var earliest int64 = 1 << 62
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
					if mt := info.ModTime().Unix(); mt < earliest {
						earliest = mt
					}
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
		stamp := route
		if earliest < (1 << 62) {
			stamp = stampFromEpoch(earliest)
		}
		audio := lastQ != "" && hasAudioFile(lastQ)
		drives = append(drives, Drive{
			Route: route, Stamp: stamp, Cameras: cams,
			HasAudio: &audio, SizeKB: sizeKB, Segments: len(segs), Location: "local",
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
  mt=$(for f in ${r}--*/*.hevc; do [ -e "$f" ] && stat -c %Y "$f"; done | sort -n | head -1)
  cams=$(ls ${r}--*/*.hevc 2>/dev/null | xargs -n1 basename 2>/dev/null | sort -u | tr "\n" "," | sed "s/,$//")
  sz=$(du -sk ${r}--* 2>/dev/null | awk "{s+=\$1} END{print s+0}")
  echo "${r}|${mt}|${cams}|${cnt}|${sz}"
done`

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
		}
		var cams []string
		for _, cam := range strings.Split(f[2], ",") {
			if cam != "" {
				cams = append(cams, labelFor(cam))
			}
		}
		sizeKB, _ := strconv.ParseInt(f[4], 10, 64)
		segs, _ := strconv.Atoi(f[3])
		drives = append(drives, Drive{
			Route: route, Stamp: stamp, Cameras: cams,
			HasAudio: nil, SizeKB: sizeKB, Segments: segs, Location: "device",
		})
	}
	return drives
}
