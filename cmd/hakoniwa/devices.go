package main

// Device name resolution and sysfs lookups.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveDevice turns a user-supplied device name into an existing
// character device path: absolute/relative paths are used as is, bare
// names are looked up under /dev and /dev/serial/by-id.
func resolveDevice(name string) (string, error) {
	candidates := []string{name}
	if !strings.Contains(name, "/") {
		candidates = []string{"/dev/" + name, "/dev/serial/by-id/" + name, "/dev/v4l/by-id/" + name}
	}
	for _, p := range candidates {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.Mode()&os.ModeCharDevice == 0 {
			return "", fmt.Errorf("%s is not a character device", p)
		}
		return p, nil
	}
	return "", fmt.Errorf("device %q not found", name)
}

// realDevice returns the canonical path of a device (symlinks resolved), used
// to detect two sessions sharing a device.
func realDevice(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// isVideoCapture reports whether p is a V4L2 device node.
func isVideoCapture(p string) bool {
	_, err := os.Stat(filepath.Join("/sys/class/video4linux", filepath.Base(realDevice(p))))
	return err == nil
}

// usbDeviceOf returns the sysfs directory of the USB device a V4L2 node
// belongs to, e.g. /sys/devices/.../usb1/1-1.
func usbDeviceOf(videoDev string) (string, error) {
	intf, err := filepath.EvalSymlinks(filepath.Join("/sys/class/video4linux", filepath.Base(realDevice(videoDev)), "device"))
	if err != nil {
		return "", fmt.Errorf("%s: not a V4L2 device: %w", videoDev, err)
	}
	dev := filepath.Dir(intf) // interface 1-1:1.0 → device 1-1
	if _, err := os.Stat(filepath.Join(dev, "busnum")); err != nil {
		return "", fmt.Errorf("%s is not a USB device", videoDev)
	}
	return dev, nil
}

// audioDeviceOf returns the ALSA capture device ("hw:CARD=<id>,DEV=0") on the
// same USB device as the capture card, or "" if there is none.
func audioDeviceOf(videoDev string) string {
	usbDev, err := usbDeviceOf(videoDev)
	if err != nil {
		return ""
	}
	cards, _ := filepath.Glob("/sys/class/sound/card[0-9]*")
	for _, c := range cards {
		intf, err := filepath.EvalSymlinks(filepath.Join(c, "device"))
		if err != nil || filepath.Dir(intf) != usbDev {
			continue
		}
		n := strings.TrimPrefix(filepath.Base(c), "card")
		if _, err := os.Stat("/proc/asound/card" + n + "/pcm0c"); err != nil {
			continue // no capture PCM
		}
		id, err := os.ReadFile("/proc/asound/card" + n + "/id")
		if err != nil {
			return "hw:" + n + ",0"
		}
		return "hw:CARD=" + strings.TrimSpace(string(id)) + ",DEV=0"
	}
	return ""
}
