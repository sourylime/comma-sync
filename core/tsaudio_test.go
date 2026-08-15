package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The device-side audio check reads only the head of a qcamera.ts, so the thing worth
// proving is that a short read still gives the same verdict ffprobe would give on the
// whole file — and that it doesn't just answer "yes" to everything.
func TestTSHasAudio(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()

	mk := func(name string, withAudio bool) string {
		p := filepath.Join(dir, name)
		args := []string{"-y", "-v", "error",
			"-f", "lavfi", "-i", "testsrc=size=640x480:rate=20:duration=6"}
		if withAudio {
			args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:duration=6",
				"-c:a", "aac", "-b:a", "64k")
		}
		args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-f", "mpegts", p)
		if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %s: %v\n%s", name, err, out)
		}
		return p
	}

	withAudio := mk("audio.ts", true)
	noAudio := mk("silent.ts", false)

	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"with audio", withAudio, true},
		{"without audio", noAudio, false},
	} {
		full, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if got, known := tsHasAudio(full); !known || got != tc.want {
			t.Errorf("%s: whole file = %v (known %v), want %v", tc.name, got, known, tc.want)
		}
		// The head is all the device sends back — it must be enough on its own.
		for _, n := range []int{qcamHeadBytes, 8 << 10, 4 << 10} {
			if n > len(full) {
				continue
			}
			if got, known := tsHasAudio(full[:n]); !known || got != tc.want {
				t.Errorf("%s: first %d bytes = %v (known %v), want %v", tc.name, n, got, known, tc.want)
			}
		}
		// Starting mid-packet must not change the answer: a truncated fetch can land
		// anywhere, and misreading the packet grid would silently produce "no audio".
		if got, known := tsHasAudio(full[97:qcamHeadBytes]); !known || got != tc.want {
			t.Errorf("%s: unaligned start = %v (known %v), want %v", tc.name, got, known, tc.want)
		}
	}

	// Junk must come back as "don't know" — never as a confident "no audio".
	for _, b := range [][]byte{nil, {0x47, 0x00}, make([]byte, 4096)} {
		if _, known := tsHasAudio(b); known {
			t.Errorf("junk input reported a known answer")
		}
	}
}
