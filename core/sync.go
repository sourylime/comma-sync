package main

import (
	"fmt"
	"time"
)

// The drives this run will touch, and how far through them we are. Sync New is given no
// list — the core discovers it — so unless the drives are counted before the work starts,
// nothing can tell the user whether they're on the first of two or the third of nine.
var (
	planRoutes []string
	planIdx    int
)

// beginPlan publishes the size of the run, before the first byte moves, so a UI can show
// the total straight away instead of inferring it as drives trickle past.
func beginPlan(routes []string) {
	planRoutes, planIdx = routes, 0
	emit(ProgressEvent{Type: "plan", Total: len(routes),
		Message: fmt.Sprintf("==> %d drive(s) to process", len(routes))})
}

// driveStep announces one drive's turn. The verb matters because with "download
// everything first" each drive comes round twice — once to transfer, once to stitch —
// and a bare "Drive 3 of 7" reappearing would look like the run had gone backwards.
func driveStep(verb, route string) {
	planIdx++
	total := len(planRoutes)
	if planIdx > total { // an unplanned drive turned up — never report x/y with x > y
		total = planIdx
	}
	// Naming the phase lets a UI count the right thing. While everything is transferring,
	// nothing is stitched yet, so a "done" tally sits at zero for the whole first pass and
	// reads as stuck; what's actually progressing is the number transferred.
	phase := ""
	switch verb {
	case "downloading":
		phase = "download"
	case "stitching":
		phase = "stitch"
	}
	emit(ProgressEvent{Type: "drive", Route: route, Index: planIdx, Total: total, Phase: phase,
		Message: fmt.Sprintf("Drive %d of %d — %s %s", planIdx, total, verb, route)})
}

// restartPlan rewinds the counter for a second pass over the same drives.
func restartPlan() { planIdx = 0 }

// localPending lists drives already on this Mac that still need stitching, skipping any
// already accounted for so a drive is never counted twice in one plan.
func localPending(counted []string) []string {
	seen := map[string]bool{}
	for _, r := range counted {
		seen[r] = true
	}
	var out []string
	for _, r := range localRoutes() {
		if seen[r] || ledgerHas(r) || !localRouteLooksComplete(r) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// cmdSync is the default flow: find the comma, download each new drive (resiliently,
// with reconnect), and stitch it ONLY once its download is verified complete — a
// partially transferred drive is left for the next run, never stitched or marked done.
func cmdSync() error {
	defer keepAwake()()
	sweepStaleTemps()

	planned := false
	host, port, cleanup, err := target()
	if err != nil {
		logf("Could not reach the comma (%v) — stitching complete local drives only.", err)
	} else {
		defer cleanup()
		c, derr := dial(host, port, 10*time.Second)
		if derr != nil {
			logf("Could not connect (%v) — stitching complete local drives only.", derr)
		} else {
			defer c.Close()
			logf("==> Comma found at %s", host)
			allFirst := syncAllFirst()

			// Settle the whole job before starting it. Drives that get skipped (already
			// synced, or still recording) are filtered out here, so the total is the number
			// actually being worked on rather than everything on the comma.
			var todo []string
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
				todo = append(todo, d.Route)
			}
			// Anything already sitting here unprocessed is part of the same run, so it counts.
			beginPlan(append(append([]string{}, todo...), localPending(todo)...))
			planned = true
			if allFirst && len(todo) > 0 {
				logf("==> Downloading every new drive first; stitching starts after the transfers.")
			}

			for _, r := range todo {
				driveStep("downloading", r)
				if e := pullRouteResilient(r, host, port); e != nil {
					emit(ProgressEvent{Type: "error", Route: r,
						Message: "download didn't finish — left for the next run: " + e.Error()})
					continue // never stitch a partial drive
				}
				emit(ProgressEvent{Type: "routedone", Route: r, Phase: "download"})
				if allFirst {
					continue // stitch later, once every transfer is done
				}
				if e := stitchRoute(r, false); e != nil {
					emit(ProgressEvent{Type: "error", Route: r, Message: e.Error()})
					continue
				}
				ledgerAdd(r)
				maybeCleanChunks(r)
				emit(ProgressEvent{Type: "routedone", Route: r})
			}
			if allFirst {
				// Same drives, second pass — count from one again so stitching reads as its
				// own progression instead of running off the end of the total.
				restartPlan()
			}
		}
	}
	if !planned {
		beginPlan(localPending(nil)) // comma unreachable: the local leftovers are the whole job
	}

	// Stitch every complete, unprocessed local drive. In all-first mode this is where
	// ALL the stitching happens (after the transfers); in per-drive mode it just picks
	// up leftovers (e.g. drives no longer on the comma). Partially downloaded drives
	// are skipped — never stitched half-finished.
	stitchCompleteLocalUnprocessed()
	emit(ProgressEvent{Type: "done", Message: "Done. Stitched drives are in: " + rootDir()})
	return nil
}

// maybeCleanChunks deletes a route's raw chunks only when CLEAN_RAW is on AND the
// stitched outputs are verified present/playable/complete — so the originals are never
// thrown away before the videos that replace them are confirmed to exist.
func maybeCleanChunks(route string) {
	if !cleanRaw() {
		return
	}
	if ok, missing := stitchedOutputsStatus(route); !ok {
		logf("      KEEPING raw chunks for %s — %s is missing, so they'd be needed to make it", route, missing)
		return
	}
	removeRouteChunks(route)
	logf("      raw chunks deleted for %s (every requested output verified)", route)
}

func stitchCompleteLocalUnprocessed() {
	for _, route := range localRoutes() {
		if ledgerHas(route) {
			continue
		}
		if !localRouteLooksComplete(route) {
			logf("==> Skipping %s: only partially downloaded — re-run with the comma connected to finish it.", route)
			continue
		}
		driveStep("stitching", route)
		if err := stitchRoute(route, false); err != nil {
			emit(ProgressEvent{Type: "error", Route: route, Message: err.Error()})
			continue
		}
		ledgerAdd(route)
		maybeCleanChunks(route)
		emit(ProgressEvent{Type: "routedone", Route: route})
	}
}

// cmdBatch processes several drives in ONE run so the core controls the ordering.
// With "download all first" (the default) it does every transfer before any stitching
// — the point being to finish pulling off the comma while it's reachable. Otherwise it
// downloads and stitches each drive in turn. Emits a "routedone" event per finished
// drive so the UIs can mark rows without needing one process per drive.
func cmdBatch(routes []string) error {
	defer keepAwake()()
	sweepStaleTemps()
	if len(routes) == 0 {
		return fmt.Errorf("no drives given")
	}

	host, port := "", 0
	if h, p, cl, err := target(); err == nil {
		host, port = h, p
		defer cl()
	}

	download := func(r string) error {
		if host != "" {
			onDev := false
			if c, derr := dial(host, port, 12*time.Second); derr == nil {
				onDev = routeOnDevice(c, r)
				c.Close()
			}
			if onDev {
				if e := pullRouteResilient(r, host, port); e != nil && !localRouteLooksComplete(r) {
					return e
				}
				return nil
			}
		}
		if localRouteLooksComplete(r) {
			return nil // already fully downloaded
		}
		if len(stitchedCameras(r)) > 0 {
			return nil // only the stitched videos remain — nothing to fetch; rebuild uses them
		}
		return fmt.Errorf("not on the comma and not fully downloaded here")
	}
	stitch := func(r string) {
		if e := stitchRoute(r, false); e != nil {
			emit(ProgressEvent{Type: "error", Route: r, Message: e.Error()})
			return
		}
		ledgerAdd(r)
		maybeCleanChunks(r)
		emit(ProgressEvent{Type: "routedone", Route: r})
	}

	beginPlan(routes)
	if syncAllFirst() && len(routes) > 1 {
		logf("==> Phase 1 of 2 — downloading %d drives (no stitching until every transfer is done)", len(routes))
		var ready []string
		for _, r := range routes {
			driveStep("downloading", r)
			if e := download(r); e != nil {
				emit(ProgressEvent{Type: "error", Route: r, Message: "download didn't finish: " + e.Error()})
				continue
			}
			emit(ProgressEvent{Type: "routedone", Route: r, Phase: "download"})
			ready = append(ready, r)
		}
		logf("==> Phase 2 of 2 — all transfers done; stitching %d drives", len(ready))
		planRoutes = ready // drives that failed to transfer aren't stitched, so don't count them
		restartPlan()
		for _, r := range ready {
			driveStep("stitching", r)
			stitch(r)
		}
	} else {
		for _, r := range routes {
			driveStep("processing", r)
			if e := download(r); e != nil {
				emit(ProgressEvent{Type: "error", Route: r, Message: "download didn't finish: " + e.Error()})
				continue
			}
			stitch(r)
		}
	}
	emit(ProgressEvent{Type: "done", Message: "Done. Stitched drives are in: " + rootDir()})
	return nil
}

// cmdDownload fetches one drive's chunks (resiliently, verified complete) WITHOUT
// stitching — the UIs use it to run a batch in two phases when "download all first"
// is on: every drive transfers while the comma is reachable, then the restitches run.
func cmdDownload(route string) error {
	defer keepAwake()()
	sweepStaleTemps()

	if host, port, cleanup, err := target(); err == nil {
		defer cleanup()
		onDev := false
		if c, derr := dial(host, port, 12*time.Second); derr == nil {
			onDev = routeOnDevice(c, route)
			c.Close()
		}
		if onDev {
			logf("==> Downloading %s", route)
			if e := pullRouteResilient(route, host, port); e != nil && !localRouteLooksComplete(route) {
				return fmt.Errorf("couldn't finish downloading %s: %w", route, e)
			}
		}
	}
	if len(localSegs(route)) == 0 {
		if len(stitchedCameras(route)) > 0 {
			emit(ProgressEvent{Type: "log", Route: route, Message: "==> " + route + " has stitched videos only — nothing to download"})
			return nil
		}
		return fmt.Errorf("no local chunks for %s and the comma isn't reachable", route)
	}
	if !localRouteLooksComplete(route) {
		return fmt.Errorf("%s is only partially downloaded — connect the comma and try again", route)
	}
	emit(ProgressEvent{Type: "log", Route: route, Message: "==> " + route + " downloaded — stitching runs after all transfers"})
	return nil
}

// cmdRestitch re-processes one drive. It first tries to (re)fetch from the comma to fill
// any gaps — this is what lets a partially-synced drive recover instead of being stuck.
// It refuses to stitch a drive that's still incomplete and can't be finished, rather
// than silently producing a partial output.
func cmdRestitch(route string) error {
	defer keepAwake()()
	sweepStaleTemps()

	if host, port, cleanup, err := target(); err == nil {
		defer cleanup()
		onDev := false
		if c, derr := dial(host, port, 12*time.Second); derr == nil {
			onDev = routeOnDevice(c, route)
			c.Close()
		}
		if onDev {
			logf("==> Fetching any missing chunks for %s from the comma…", route)
			if e := pullRouteResilient(route, host, port); e != nil && !localRouteLooksComplete(route) {
				return fmt.Errorf("couldn't finish downloading %s from the comma: %w", route, e)
			}
		}
	}

	if len(localSegs(route)) == 0 {
		// No chunks and not on the comma — but if the stitched per-camera videos survive
		// in the output folder, rebuild the derived outputs from them.
		if rebuildExtrasFromStitched(route) {
			emit(ProgressEvent{Type: "done", Message: "Rebuilt from stitched videos. Output in: " + rootDir()})
			return nil
		}
		return fmt.Errorf("no local chunks for %s and the comma isn't reachable", route)
	}
	if !localRouteLooksComplete(route) {
		return fmt.Errorf("%s is only partially downloaded and the comma isn't reachable to finish it — connect and try again", route)
	}
	if err := stitchRoute(route, false); err != nil {
		return err
	}
	ledgerAdd(route)
	emit(ProgressEvent{Type: "done", Message: "Re-sync complete. Output in: " + rootDir()})
	return nil
}
