// update.go — "is there a newer release?" check, shared by every front-end.
// Reads GitHub's public releases list only; sends nothing about the user.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const coreVersion = "0.2.0"

// Default repo to check. Forks can override with --repo owner/name.
const defaultRepo = "sourylime/comma-sync"

type ghRelease struct {
	TagName    string `json:"tag_name"`
	HTMLURL    string `json:"html_url"`
	Prerelease bool   `json:"prerelease"`
	Draft      bool   `json:"draft"`
}

type updateResult struct {
	UpdateAvailable bool   `json:"updateAvailable"`
	Current         string `json:"current"`
	Latest          string `json:"latest,omitempty"`
	Tag             string `json:"tag,omitempty"`
	URL             string `json:"url,omitempty"`
}

// cmdUpdateCheck parses its own flags from the raw arg list:
//   --current X.Y.Z   version to compare against (default: this core's version)
//   --repo owner/name (default: sourylime/comma-sync)
//   --prefix gui-v    only consider releases whose tag starts with this
//   --prereleases     include pre-releases (the GUI beta lives in these)
func cmdUpdateCheck(args []string) {
	repo := defaultRepo
	prefix := ""
	current := coreVersion
	includePre := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--current":
			if i+1 < len(args) {
				current = args[i+1]
				i++
			}
		case "--repo":
			if i+1 < len(args) {
				repo = args[i+1]
				i++
			}
		case "--prefix":
			if i+1 < len(args) {
				prefix = args[i+1]
				i++
			}
		case "--prereleases":
			includePre = true
		}
	}

	res := updateResult{Current: current}
	rels, err := fetchReleases(repo)
	if err != nil {
		// Never fail the caller on a network hiccup — just report "no update".
		emitUpdate(res, err)
		return
	}

	var bestVer, bestTag, bestURL string
	for _, r := range rels {
		if r.Draft || (r.Prerelease && !includePre) {
			continue
		}
		if prefix != "" && !strings.HasPrefix(r.TagName, prefix) {
			continue
		}
		ver := cleanVersion(strings.TrimPrefix(r.TagName, prefix))
		if bestVer == "" || versionGreater(ver, bestVer) {
			bestVer, bestTag, bestURL = ver, r.TagName, r.HTMLURL
		}
	}
	if bestVer != "" && versionGreater(bestVer, cleanVersion(current)) {
		res.UpdateAvailable = true
		res.Latest, res.Tag, res.URL = bestVer, bestTag, bestURL
	}
	emitUpdate(res, nil)
}

func emitUpdate(res updateResult, err error) {
	if jsonProgress {
		out, _ := json.Marshal(res)
		fmt.Println(string(out))
		return
	}
	if err != nil {
		fmt.Println("Update check unavailable:", err)
	} else if res.UpdateAvailable {
		fmt.Printf("Update available: %s  %s\n", res.Tag, res.URL)
	} else {
		fmt.Println("Up to date (" + res.Current + ")")
	}
}

func fetchReleases(repo string) ([]ghRelease, error) {
	req, _ := http.NewRequest("GET",
		"https://api.github.com/repos/"+repo+"/releases?per_page=50", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "comma-sync")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github api returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var rels []ghRelease
	if err := json.Unmarshal(body, &rels); err != nil {
		return nil, err
	}
	return rels, nil
}

// versionGreater compares dotted numeric versions (1.0.3 > 1.0.2), ignoring any
// non-numeric suffix on a component (e.g. "1-beta" reads as 1).
func versionGreater(a, b string) bool {
	pa, pb := versionParts(a), versionParts(b)
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// cleanVersion drops a leading "v" and any pre-release suffix ("1.0.1-beta.2" ->
// "1.0.1"), so updates are keyed on the numeric version — bump the number (not just
// the beta suffix) to make a new build show up as an update.
func cleanVersion(s string) string {
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s = s[:i]
	}
	return s
}

func versionParts(s string) []int {
	var out []int
	for _, p := range strings.Split(s, ".") {
		num := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				break
			}
			num = num*10 + int(c-'0')
		}
		out = append(out, num)
	}
	return out
}
