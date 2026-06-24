package main

import (
	"bufio"
	"os"
	"strings"
)

// The ledger (.processed_routes) records routes already stitched, so they're never
// re-downloaded or re-stitched. Same file/format the bash script uses.

func ledgerHas(route string) bool {
	f, err := os.Open(ledgerPath())
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == route {
			return true
		}
	}
	return false
}

func ledgerAdd(route string) {
	if ledgerHas(route) {
		return
	}
	_ = os.MkdirAll(rootDir(), 0o755)
	f, err := os.OpenFile(ledgerPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(route + "\n")
}
