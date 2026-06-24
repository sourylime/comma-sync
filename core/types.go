package main

// Drive describes one recorded drive (route), matching the macOS app's expectations.
type Drive struct {
	Route    string   `json:"route"`
	Stamp    string   `json:"stamp"`     // recording start, YYYY-MM-DD_HH-MM-SS
	Cameras  []string `json:"cameras"`   // friendly labels: road/wide/driver
	HasAudio *bool    `json:"hasAudio"`  // nil = unknown (device-only, not probed)
	SizeKB   int64    `json:"sizeKB"`
	Segments int      `json:"segments"`
	Location string   `json:"location"`  // "local" (chunks on disk) | "device"
}

// ProgressEvent is the structured stream `sync` will emit (one JSON object per line)
// so GUIs never have to parse human text. Used once sync/restitch are implemented.
type ProgressEvent struct {
	Type    string  `json:"type"`              // progress | log | drive | done | error
	Route   string  `json:"route,omitempty"`
	Phase   string  `json:"phase,omitempty"`   // download | stitch
	Percent float64 `json:"percent,omitempty"`
	Message string  `json:"message,omitempty"`
}
