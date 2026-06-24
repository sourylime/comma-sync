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
