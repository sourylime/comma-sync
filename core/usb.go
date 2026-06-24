package main

import (
	"fmt"
	"os/exec"
)

// setupUSB tunnels SSH over the comma's ADB gadget (adb forward). Returns a
// cleanup func. Not faster than WiFi on the comma 3X — a no-WiFi fallback only.
func setupUSB() (func(), error) {
	if _, err := exec.LookPath("adb"); err != nil {
		return nil, fmt.Errorf("USE_USB=1 needs adb (macOS: brew install --cask android-platform-tools · Linux: apt install android-tools-adb)")
	}
	if err := exec.Command("adb", "get-state").Run(); err != nil {
		return nil, fmt.Errorf("no ADB device — enable ADB on the comma (Settings → Developer) and connect USB")
	}
	fwd := fmt.Sprintf("tcp:%d", usbPort())
	if err := exec.Command("adb", "forward", fwd, fmt.Sprintf("tcp:%d", commaPort())).Run(); err != nil {
		return nil, fmt.Errorf("adb forward failed: %w", err)
	}
	logf("Using USB (ADB) link. NOTE: not faster than WiFi on the comma 3X — fallback only.")
	return func() { _ = exec.Command("adb", "forward", "--remove", fwd).Run() }, nil
}

// target returns the host+port to connect to (USB tunnel if USE_USB, else the
// discovered WiFi IP) plus a cleanup func.
func target() (string, int, func(), error) {
	if useUSB() {
		cleanup, err := setupUSB()
		if err != nil {
			return "", 0, nil, err
		}
		return "127.0.0.1", usbPort(), cleanup, nil
	}
	ip, err := discover()
	if err != nil {
		return "", 0, nil, err
	}
	return ip, commaPort(), func() {}, nil
}
