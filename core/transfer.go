package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// progressCounter throttles download progress events for one route.
type progressCounter struct {
	route      string
	total, done int64
	last       time.Time
}

func (p *progressCounter) add(n int) {
	p.done += int64(n)
	if time.Since(p.last) > 300*time.Millisecond {
		p.last = time.Now()
		pct := 0.0
		if p.total > 0 {
			pct = float64(p.done) / float64(p.total) * 100
		}
		emit(ProgressEvent{Type: "progress", Route: p.route, Phase: "download", Percent: pct})
	}
}

type countWriter struct {
	w  io.Writer
	pc *progressCounter
}

func (c countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.pc.add(n)
	return n, err
}

// throttleWriter caps throughput to rate bytes/sec (0 = unlimited) by sleeping —
// BWLIMIT uses it to lower the comma's power draw on a weak supply.
type throttleWriter struct {
	w       io.Writer
	rate    int64
	written int64
	start   time.Time
}

func (t *throttleWriter) Write(b []byte) (int, error) {
	n, err := t.w.Write(b)
	if t.rate > 0 {
		t.written += int64(n)
		want := float64(t.written) / float64(t.rate)
		if got := time.Since(t.start).Seconds(); want > got {
			time.Sleep(time.Duration((want - got) * float64(time.Second)))
		}
	}
	return n, err
}

func wantFile(name string) bool {
	return strings.HasSuffix(name, ".hevc") || name == "qcamera.ts"
}

// pullRoute downloads a route's hevc + qcamera.ts into chunksDir over SFTP, with
// resume (skip complete files, append partial) and device-mtime preservation
// (the recording-start stamp depends on those mtimes).
func pullRoute(c *ssh.Client, route string) error {
	sc, err := sftp.NewClient(c)
	if err != nil {
		return err
	}
	defer sc.Close()

	entries, err := sc.ReadDir(remotePath)
	if err != nil {
		return err
	}
	type rfile struct {
		rpath, lpath string
		size         int64
		mtime        time.Time
	}
	var files []rfile
	var total, have int64
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m := segRe.FindStringSubmatch(e.Name())
		if m == nil || m[1] != route {
			continue
		}
		seg := e.Name()
		segFiles, err := sc.ReadDir(remotePath + seg)
		if err != nil {
			continue
		}
		for _, fi := range segFiles {
			if !wantFile(fi.Name()) {
				continue
			}
			lpath := filepath.Join(chunksDir(), seg, fi.Name())
			files = append(files, rfile{remotePath + seg + "/" + fi.Name(), lpath, fi.Size(), fi.ModTime()})
			total += fi.Size()
			if st, err := os.Stat(lpath); err == nil {
				if st.Size() > fi.Size() {
					have += fi.Size()
				} else {
					have += st.Size()
				}
			}
		}
	}
	if len(files) == 0 {
		return fmt.Errorf("route %s not found on the comma", route)
	}

	pc := &progressCounter{route: route, total: total, done: have}
	for _, f := range files {
		if err := pullFile(sc, f.rpath, f.lpath, f.size, f.mtime, pc); err != nil {
			return err
		}
	}
	emit(ProgressEvent{Type: "progress", Route: route, Phase: "download", Percent: 100})
	return nil
}

func pullFile(sc *sftp.Client, rpath, lpath string, size int64, mtime time.Time, pc *progressCounter) error {
	if st, err := os.Stat(lpath); err == nil && st.Size() == size {
		return nil // already complete
	}
	if err := os.MkdirAll(filepath.Dir(lpath), 0o755); err != nil {
		return err
	}
	var offset int64
	if st, err := os.Stat(lpath); err == nil && st.Size() < size {
		offset = st.Size()
	}
	rf, err := sc.Open(rpath)
	if err != nil {
		return err
	}
	defer rf.Close()

	flags := os.O_CREATE | os.O_WRONLY
	if offset > 0 {
		flags |= os.O_APPEND
		if _, err := rf.Seek(offset, io.SeekStart); err != nil {
			return err
		}
	} else {
		flags |= os.O_TRUNC
	}
	lf, err := os.OpenFile(lpath, flags, 0o644)
	if err != nil {
		return err
	}
	var dst io.Writer = lf
	if rate := bwLimitBytesPerSec(); rate > 0 {
		dst = &throttleWriter{w: lf, rate: rate, start: time.Now()}
	}
	_, err = io.Copy(countWriter{dst, pc}, rf)
	lf.Close()
	if err != nil {
		return err
	}
	_ = os.Chtimes(lpath, mtime, mtime) // preserve device mtime → correct stamp
	return nil
}

// remoteNewestMtime returns the newest hevc mtime for a route (for the
// "still recording?" check), or 0.
func remoteNewestMtime(c *ssh.Client, route string) int64 {
	cmd := fmt.Sprintf(`for f in %s%s--*/*.hevc; do [ -e "$f" ] && stat -c %%Y "$f"; done | sort -n | tail -1`, remotePath, route)
	out, _ := runCmd(c, cmd)
	n, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	return n
}
