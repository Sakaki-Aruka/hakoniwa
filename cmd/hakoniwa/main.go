// Command hakoniwa is an IP-KVM: it streams a target PC's screen and audio
// from a USB HDMI capture device to the browser and sends keyboard/mouse
// input to the target through an ESP32-S3 USB HID bridge.
//
//	hakoniwa start [options] <esp32-device> <video-device> [port]
//	hakoniwa close [port]
//	hakoniwa list
//	hakoniwa serve [flags]
package main

import (
	"fmt"
	"os"
	"unsafe"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

// The V4L2 and usbfs structures in this package are laid out for 64-bit
// Linux; this fails to compile on 32-bit targets.
var _ = [1]struct{}{}[unsafe.Sizeof(uintptr(0))/8-1]

const usageText = `hakoniwa - IP-KVM (HDMI capture + ESP32-S3 USB HID)

Usage:
  hakoniwa start [options] <esp32-device> <video-device> [port]
      Start a session in the background. port defaults to 8080.
      Devices may be given as full paths or as names under /dev
      (e.g. ttyACM0 video0).
      Options:
        --bind ADDR      address to listen on (default 127.0.0.1; there is
                         no authentication, so expose with care)
        --size WxH       initial resolution (default 1920x1080)
        --audio DEV      ALSA capture device, "auto" (default) or "none"
        --backend NAME   v4l2 (default) or usbfs
  hakoniwa close [port]
      Stop the session on port, or every session when port is omitted.
  hakoniwa list
      Show running sessions.
  hakoniwa serve [flags]
      Run a session in the foreground (see "hakoniwa serve -h").
  hakoniwa version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "start":
		os.Exit(cmdStart(args))
	case "close", "stop":
		os.Exit(cmdClose(args))
	case "list", "ls":
		os.Exit(cmdList(args))
	case "serve":
		os.Exit(runServe(args))
	case "version", "--version":
		fmt.Println("hakoniwa", version)
	case "help", "-h", "--help":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "hakoniwa: unknown command %q\n\n%s", cmd, usageText)
		os.Exit(2)
	}
}
