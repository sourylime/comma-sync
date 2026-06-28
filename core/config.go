package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const remoteUser = "comma"
const remotePath = "/data/media/0/realdata/"

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func rootDir() string  { return envOr("ROOT", "Comma Footage") }
func chunksDir() string {
	if v := os.Getenv("CHUNKS_DIR"); v != "" {
		return v
	}
	return filepath.Join(rootDir(), "Raw HEVC Chunks")
}
func ipCachePath() string { return filepath.Join(rootDir(), ".last_ip") }
func ledgerPath() string  { return filepath.Join(rootDir(), ".processed_routes") }
func withAudio() bool     { return envOr("WITH_AUDIO", "1") != "0" }

// Combined multi-angle video: roles by label (road|wide|driver).
func withCombined() bool   { return os.Getenv("WITH_COMBINED") == "1" }
func primaryCam() string   { return envOr("PRIMARY_CAM", "road") }
func secondaryCam() string { return envOr("SECONDARY_CAM", "wide") }
func tertiaryCam() string  { return envOr("TERTIARY_CAM", "driver") }

func commaPort() int {
	if n, err := strconv.Atoi(os.Getenv("REMOTE_PORT")); err == nil && n > 0 {
		return n
	}
	return 22
}

// label maps a camera filename to a friendly name (mirrors comma-sync.sh).
func labelFor(cam string) string {
	switch cam {
	case "fcamera.hevc":
		return "road"
	case "ecamera.hevc":
		return "wide"
	case "dcamera.hevc":
		return "driver"
	default:
		return strings.TrimSuffix(cam, ".hevc")
	}
}

func joinCams(c []string) string { return strings.Join(c, ",") }

func cleanRaw() bool   { return os.Getenv("CLEAN_RAW") == "1" }
func skipLatest() bool { return envOr("SKIP_LATEST", "1") != "0" }
func useUSB() bool     { return os.Getenv("USE_USB") == "1" }
func fps() string      { return envOr("FPS", "20") }

// bwLimitBytesPerSec parses BWLIMIT ("3m" = 3 MB/s, "500k", bare = KB/s) to bytes/sec;
// 0 = unlimited. Throttling lowers the comma's power draw on weak supplies.
func bwLimitBytesPerSec() int64 {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("BWLIMIT")))
	if v == "" {
		return 0
	}
	mult := int64(1024) // bare number = KB/s (rsync convention)
	switch {
	case strings.HasSuffix(v, "m"):
		mult, v = 1024*1024, strings.TrimSuffix(v, "m")
	case strings.HasSuffix(v, "k"):
		mult, v = 1024, strings.TrimSuffix(v, "k")
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || f <= 0 {
		return 0
	}
	return int64(f * float64(mult))
}

func minAgeSecs() int64 {
	if n, err := strconv.ParseInt(envOr("MIN_AGE_SECS", "120"), 10, 64); err == nil {
		return n
	}
	return 120
}

func usbPort() int {
	if n, err := strconv.Atoi(envOr("USB_PORT", "2222")); err == nil && n > 0 {
		return n
	}
	return 2222
}
