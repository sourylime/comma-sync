// comma-sync core — a single cross-platform binary backing the front-ends.
// The bash script and macOS app are unaffected; this is built alongside them.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func usage() {
	fmt.Fprintln(os.Stderr, `comma-sync (core)

Usage:
  comma-sync discover               Find the comma, print its IP (JSON)
  comma-sync list [--json]          List drives on this computer + still on the comma
  comma-sync sync [--json]          Download new drives and stitch them
  comma-sync restitch <route> [--json]   Re-stitch one drive (re-downloads if needed)
  comma-sync update-check [--json]  Check GitHub for a newer release
  comma-sync version

Env: ROOT, CHUNKS_DIR, COMMA_IP, REMOTE_PORT, SSH_KEY, WITH_AUDIO,
     CLEAN_RAW, SKIP_LATEST, MIN_AGE_SECS, FPS, USE_USB, USB_PORT`)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "!! "+err.Error())
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	// shared flag parsing
	var positional []string
	for _, a := range os.Args[2:] {
		switch {
		case a == "--json":
			jsonProgress = true
		case strings.HasPrefix(a, "-"):
			// ignore unknown flags for now
		default:
			positional = append(positional, a)
		}
	}

	switch os.Args[1] {
	case "discover":
		host, _, cleanup, err := target()
		if err != nil {
			fail(err)
		}
		cleanup()
		transport := "wifi"
		if useUSB() {
			transport = "usb"
		}
		out, _ := json.Marshal(map[string]string{"ip": host, "transport": transport})
		fmt.Println(string(out))

	case "list":
		drives := listDrives()
		if jsonProgress {
			out, _ := json.Marshal(drives)
			fmt.Println(string(out))
		} else {
			for _, d := range drives {
				fmt.Printf("%s\t%s\t%s\t%d KB\t%d min\t[%s]\n",
					d.Route, d.Stamp, joinCams(d.Cameras), d.SizeKB, d.Segments, d.Location)
			}
		}

	case "sync":
		if err := cmdSync(); err != nil {
			fail(err)
		}

	case "restitch":
		if len(positional) == 0 {
			fmt.Fprintln(os.Stderr, "usage: comma-sync restitch <route>")
			os.Exit(2)
		}
		if err := cmdRestitch(positional[0]); err != nil {
			fail(err)
		}

	case "update-check":
		cmdUpdateCheck(os.Args[2:])

	case "version":
		fmt.Println("comma-sync core " + coreVersion)

	default:
		usage()
		os.Exit(2)
	}
}
