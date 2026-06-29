package main

import (
	"fmt"
	"time"
)

// cmdSync is the default flow: find the comma, download new drives (those not in
// the ledger), then stitch every local drive that isn't processed yet.
func cmdSync() error {
	defer keepAwake()()
	sweepStaleTemps()
	host, port, cleanup, err := target()
	if err != nil {
		logf("Could not reach the comma (%v) — stitching local chunks only.", err)
		return stitchUnprocessedLocal()
	}
	defer cleanup()
	c, err := dial(host, port, 10*time.Second)
	if err != nil {
		logf("Could not connect (%v) — stitching local chunks only.", err)
		return stitchUnprocessedLocal()
	}
	defer c.Close()
	logf("==> Comma found at %s", host)

	for _, d := range listDeviceWith(c) {
		if ledgerHas(d.Route) {
			continue
		}
		if skipLatest() && minAgeSecs() > 0 {
			if newest := remoteNewestMtime(c, d.Route); newest > 0 && time.Now().Unix()-newest < minAgeSecs() {
				logf("==> Skipping %s: still recording (re-run later).", d.Route)
				continue
			}
		}
		logf("==> Downloading %s", d.Route)
		if err := pullRoute(c, d.Route); err != nil {
			emit(ProgressEvent{Type: "error", Route: d.Route, Message: "download failed: " + err.Error()})
		}
	}
	return stitchUnprocessedLocal()
}

func stitchUnprocessedLocal() error {
	for _, route := range localRoutes() {
		if ledgerHas(route) {
			continue
		}
		if err := stitchRoute(route, false); err != nil {
			emit(ProgressEvent{Type: "error", Route: route, Message: err.Error()})
			continue
		}
		ledgerAdd(route)
		if cleanRaw() {
			removeRouteChunks(route)
		}
	}
	emit(ProgressEvent{Type: "done", Message: "Done. Stitched drives are in: " + rootDir()})
	return nil
}

// cmdRestitch re-stitches one drive with collision-safe naming, re-downloading
// its chunks from the comma first if they're not on disk.
func cmdRestitch(route string) error {
	defer keepAwake()()
	sweepStaleTemps()
	if len(localSegs(route)) == 0 {
		host, port, cleanup, err := target()
		if err != nil {
			return fmt.Errorf("no local chunks and can't reach the comma: %w", err)
		}
		defer cleanup()
		c, err := dial(host, port, 10*time.Second)
		if err != nil {
			return err
		}
		defer c.Close()
		logf("==> Downloading %s from the comma...", route)
		if err := pullRoute(c, route); err != nil {
			return err
		}
	}
	if err := stitchRoute(route, true); err != nil {
		return err
	}
	emit(ProgressEvent{Type: "done", Message: "Re-sync complete. Output in: " + rootDir()})
	return nil
}
