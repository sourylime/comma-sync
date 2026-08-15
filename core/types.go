package main

// Drive describes one recorded drive (route), matching the macOS app's expectations.
type Drive struct {
	Route    string   `json:"route"`
	Stamp    string   `json:"stamp"`    // recording start, YYYY-MM-DD_HH-MM-SS
	Cameras  []string `json:"cameras"`  // friendly labels: road/wide/driver
	HasAudio *bool    `json:"hasAudio"` // nil = unknown (device-only, not probed)
	SizeKB   int64    `json:"sizeKB"`
	Segments int      `json:"segments"`
	Location string   `json:"location"` // "local" (chunks on disk) | "device" | "stitched"
	// Synced means this drive's videos are already in the output folder. It's separate
	// from Location because the two are independent: a drive can still be on the comma
	// AND already be stitched here, and showing only "on comma" made finished work look
	// like work still to do.
	Synced bool `json:"synced"`
}

// ProgressEvent is the structured stream `sync` will emit (one JSON object per line)
// so GUIs never have to parse human text. Used once sync/restitch are implemented.
type ProgressEvent struct {
	Type     string  `json:"type"` // progress | log | plan | drive | routedone | done | error
	Route    string  `json:"route,omitempty"`
	Phase    string  `json:"phase,omitempty"` // download | stitch
	Percent  float64 `json:"percent,omitempty"`
	RateMBps float64 `json:"rateMBps,omitempty"` // live download throughput (smoothed)
	Message  string  `json:"message,omitempty"`
	// Index/Total place a drive within the whole run ("drive 3 of 7"). Only the core can
	// count that — it's the side that lists the comma and filters out what's already
	// synced — so it tells the UIs rather than them guessing. Total also arrives once as
	// a "plan" event, before any work starts, so the count is known from the first second.
	Index int `json:"index,omitempty"`
	Total int `json:"total,omitempty"`
}
