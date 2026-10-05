# hakoniwa

A DIY IP-KVM: view a target PC's screen and audio in the browser and control
it with your keyboard and mouse.

- **Video/audio:** a USB HDMI capture device (MJPEG, UVC) on a Linux host.
- **Keyboard/mouse:** an ESP32-S3 that appears to the target as a USB HID
  (boot keyboard, boot mouse, absolute mouse), driven over its UART port.
- **Browser UI:** low-latency video over WebSocket, HDMI audio, keyboard and
  mouse capture (absolute and pointer-lock relative modes), English/Japanese.

## Build

hakoniwa is pure Go (no cgo) and cross-compiles to any 64-bit Linux:

```
make build    # ./hakoniwa
make dist     # dist/hakoniwa-linux-{amd64,arm64,riscv64}
```

or `go install github.com/Sakaki-Aruka/hakoniwa/cmd/hakoniwa@latest`.

Audio capture uses `arecord` (alsa-utils) when available.

The ESP32-S3 firmware is in `esp32-hid/` (ESP-IDF + TinyUSB, built with
PlatformIO: `cd esp32-hid && pio run -t upload`).

## Usage

```
hakoniwa start [options] <esp32-device> <video-device> [port]   # default port 8080
hakoniwa list
hakoniwa close [port]                                           # all sessions if omitted
```

Example:

```
$ hakoniwa start ttyACM0 video0
started session on http://127.0.0.1:8080/ (pid 12345)
```

Options for `start`: `--bind ADDR` (default `127.0.0.1`), `--size WxH`,
`--audio auto|none|DEVICE`, `--backend v4l2|usbfs`.

There is no authentication yet: keep the default `--bind 127.0.0.1` unless the
network is trusted.

## License

MIT. See [LICENSE](LICENSE).
