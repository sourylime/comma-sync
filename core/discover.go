package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// discoverMDNS finds the comma by name via mDNS — no subnet scan, gentle on weak
// WiFi, and it keeps working when the DHCP IP changes. The comma's avahi advertises
// an _ssh._tcp service named like "comma SSH - tizi - [comma-XXXX]". Returns its
// .local hostname, or "" if not found / no mDNS tool is available.
func discoverMDNS() string {
	if path, err := exec.LookPath("dns-sd"); err == nil { // macOS
		cmd := exec.Command(path, "-B", "_ssh._tcp", "local")
		stdout, err := cmd.StdoutPipe()
		if err != nil || cmd.Start() != nil {
			return ""
		}
		re := regexp.MustCompile(`\[(comma-[A-Za-z0-9]+)\]`)
		found := make(chan string, 1)
		go func() {
			sc := bufio.NewScanner(stdout)
			for sc.Scan() {
				if m := re.FindStringSubmatch(sc.Text()); m != nil {
					found <- m[1]
					return
				}
			}
			found <- ""
		}()
		var host string
		select {
		case host = <-found:
		case <-time.After(2 * time.Second):
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if host != "" {
			return host + ".local"
		}
	} else if path, err := exec.LookPath("avahi-browse"); err == nil { // Linux
		out, _ := exec.Command(path, "-rtp", "_ssh._tcp").Output()
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "=") && strings.Contains(strings.ToLower(line), "comma") {
				if f := strings.Split(line, ";"); len(f) >= 7 {
					return f[6]
				}
			}
		}
	}
	return ""
}

func portOpen(host string, port int, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(port)), timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// isComma confirms a host is our device by SSHing in and checking for openpilot.
func isComma(host string, port int) bool {
	c, err := dial(host, port, 4*time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	_, err = runCmd(c, "test -d /data/openpilot")
	return err == nil
}

// localIPv4 returns this machine's private LAN IPv4 (for deriving the /24 to scan).
func localIPv4() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ip4 := ipnet.IP.To4(); ip4 != nil && ip4.IsPrivate() {
				return ip4.String()
			}
		}
	}
	return ""
}

// discover finds the comma's current IP: try COMMA_IP and the cached IP first
// (cheap), then scan the local /24. Caches and returns the winner.
func discover() (string, error) {
	port := commaPort()

	var candidates []string
	if ip := os.Getenv("COMMA_IP"); ip != "" {
		candidates = append(candidates, ip)
	}
	if b, err := os.ReadFile(ipCachePath()); err == nil {
		candidates = append(candidates, strings.TrimSpace(string(b)))
	}
	for _, ip := range candidates {
		if ip != "" && portOpen(ip, port, time.Second) && isComma(ip, port) {
			cacheIP(ip)
			return ip, nil
		}
	}

	// Preferred: mDNS by name — no subnet scan, survives DHCP changes.
	if host := discoverMDNS(); host != "" && isComma(host, port) {
		cacheIP(host)
		fmt.Fprintf(os.Stderr, "==> Comma found via mDNS (%s)\n", host)
		return host, nil
	}

	// AUTO_DISCOVER=0 skips the subnet scan (which can disrupt weak WiFi) — only
	// COMMA_IP / cache / mDNS are used. Matches comma-sync.sh.
	if os.Getenv("AUTO_DISCOVER") == "0" {
		return "", fmt.Errorf("comma not found (AUTO_DISCOVER off; set COMMA_IP or use mDNS)")
	}

	local := localIPv4()
	if local == "" {
		return "", fmt.Errorf("couldn't determine this machine's LAN address")
	}
	base := local[:strings.LastIndex(local, ".")]
	fmt.Fprintf(os.Stderr, "==> Scanning %s.0/24 for the comma (port %d)...\n", base, port)

	open := scanSubnet(base, port)
	sort.Slice(open, func(i, j int) bool { return lastOctet(open[i]) < lastOctet(open[j]) })
	for _, ip := range open {
		if ip == local {
			continue
		}
		if isComma(ip, port) {
			cacheIP(ip)
			return ip, nil
		}
	}
	return "", fmt.Errorf("no comma found on %s.0/24 (is it powered on and on this WiFi?)", base)
}

// scanSubnet returns hosts on base.X with the SSH port open (concurrent).
func scanSubnet(base string, port int) []string {
	var (
		mu   sync.Mutex
		open []string
		wg   sync.WaitGroup
	)
	sem := make(chan struct{}, 64)
	for i := 1; i <= 254; i++ {
		wg.Add(1)
		ip := fmt.Sprintf("%s.%d", base, i)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if portOpen(ip, port, time.Second) {
				mu.Lock()
				open = append(open, ip)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return open
}

func cacheIP(ip string) {
	_ = os.MkdirAll(rootDir(), 0o755)
	_ = os.WriteFile(ipCachePath(), []byte(ip), 0o644)
}

func lastOctet(ip string) int {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return 0
	}
	n := 0
	fmt.Sscanf(parts[3], "%d", &n)
	return n
}
