package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The audio track is built from every segment at once now, so the thing that has to be
// nailed down is the assembly: each segment must occupy exactly its own slot, in order,
// with a late microphone pushed back by silence rather than sliding the rest of the
// drive. Getting any of that wrong is what "the audio drifts after N minutes" looks like.
func TestBuildAudioPCMLayout(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()

	const (
		segSecs   = 3.0
		frames    = 60 // 3s at 20fps
		slotBytes = int64(segSecs * audioRate * 2)
		micLate   = 1.0 // seconds the mic is late in segment 0
	)

	segs := []string{"testroute--0", "testroute--1", "testroute--2"}
	for _, s := range segs {
		if err := os.MkdirAll(filepath.Join(dir, s), 0o755); err != nil {
			t.Fatal(err)
		}
		// Every segment has video, so every segment owns a slot in the track.
		run(t, "ffmpeg", "-y", "-v", "error",
			"-f", "lavfi", "-i", "testsrc=size=160x120:rate=20:duration=3",
			"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=none",
			"-f", "hevc", filepath.Join(dir, s, "fcamera.hevc"))
	}
	// Segment 0: the microphone starts a second late, as it does at the top of a drive.
	run(t, "ffmpeg", "-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=20:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-af", "adelay=1000|1000", "-c:v", "libx264", "-preset", "ultrafast",
		"-c:a", "aac", "-f", "mpegts", filepath.Join(dir, segs[0], "qcamera.ts"))
	// Segment 1: audio all the way through.
	run(t, "ffmpeg", "-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=20:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-f", "mpegts",
		filepath.Join(dir, segs[1], "qcamera.ts"))
	// Segment 2: no qcamera.ts at all — its slot must still exist, as silence.

	ref := map[string]int{segs[0]: frames, segs[1]: frames, segs[2]: frames}

	pcm, dur := buildAudioPCM(dir, segs, ref)
	if pcm == "" {
		t.Fatal("no audio track produced")
	}
	defer os.Remove(pcm)
	data, err := os.ReadFile(pcm)
	if err != nil {
		t.Fatal(err)
	}

	// Exact length: the track has to be as long as the video, to the byte. Anything
	// else means some segment's slot was mis-sized and everything after it shifts.
	want := slotBytes * int64(len(segs))
	if int64(len(data)) != want {
		t.Errorf("track is %d bytes, want %d (%.2fs vs %.2fs)",
			len(data), want, dur, segSecs*float64(len(segs)))
	}

	silent := func(b []byte) bool { return bytes.Equal(b, make([]byte, len(b))) }
	slot := func(i int) []byte { return data[int64(i)*slotBytes : int64(i+1)*slotBytes] }

	// The late mic is held back by silence rather than starting the drive early.
	lead := int64(micLate*audioRate*2) - 4000 // just short of the gap, to allow rounding
	if !silent(slot(0)[:lead]) {
		t.Error("segment 0 should open with silence while the microphone is still starting")
	}
	if silent(slot(0)[int64(micLate*audioRate*2)+8000:]) {
		t.Error("segment 0 has no audio after the microphone starts")
	}
	// A segment with full audio must not be padded at the front.
	if silent(slot(1)[:8000]) {
		t.Error("segment 1 should start with audio, not silence")
	}
	// A segment with no qcamera.ts still holds its place.
	if !silent(slot(2)) {
		t.Error("segment 2 has no audio source, so its slot must be silence")
	}

	// Building it again must give the same bytes: the work is spread across goroutines,
	// and a race in the assembly would show up as a track that changes between runs.
	for i := 0; i < 2; i++ {
		p2, _ := buildAudioPCM(dir, segs, ref)
		d2, _ := os.ReadFile(p2)
		os.Remove(p2)
		if !bytes.Equal(data, d2) {
			t.Fatalf("run %d produced a different track", i+2)
		}
	}
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, out)
	}
}
