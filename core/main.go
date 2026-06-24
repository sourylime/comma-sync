// comma-sync core — a single cross-platform binary that will eventually back all
// the front-ends. WORK IN PROGRESS: `discover` and `list` are implemented; `sync`
// and `restitch` are stubbed (use ../comma-sync.sh for those until they land).
//
// The existing bash script and macOS app are unaffected — this is built alongside.
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func usage() {
	fmt.Fprintln(os.Stderr, `comma-sync (core, WIP)

Usage:
  comma-sync discover            Find the comma on the network, print its IP
  comma-sync list [--json]       List drives on this computer + still on the comma
  comma-sync sync [routes...]    (not yet implemented — use ../comma-sync.sh)
  comma-sync restitch <route>    (not yet implemented — use ../comma-sync.sh)
  comma-sync version

Env: ROOT, CHUNKS_DIR, COMMA_IP, REMOTE_PORT, SSH_KEY, WITH_AUDIO`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "discover":
		ip, err := discover()
		if err != nil {
			fmt.Fprintln(os.Stderr, "!! "+err.Error())
			os.Exit(1)
		}
		out, _ := json.Marshal(map[string]string{"ip": ip, "transport": "wifi"})
		fmt.Println(string(out))

	case "list":
		jsonOut := false
		for _, a := range os.Args[2:] {
			if a == "--json" {
				jsonOut = true
			}
		}
		drives := listDrives()
		if jsonOut {
			out, _ := json.Marshal(drives)
			fmt.Println(string(out))
		} else {
			for _, d := range drives {
				fmt.Printf("%s\t%s\t%s\t%d KB\t%d min\t[%s]\n",
					d.Route, d.Stamp, joinCams(d.Cameras), d.SizeKB, d.Segments, d.Location)
			}
		}

	case "sync", "restitch":
		fmt.Fprintln(os.Stderr, "!! '"+os.Args[1]+"' is not implemented in the core yet — use ../comma-sync.sh for now.")
		os.Exit(3)

	case "version":
		fmt.Println("comma-sync core 0.1.0-dev")

	default:
		usage()
		os.Exit(2)
	}
}
