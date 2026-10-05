package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"log/syslog"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Source produces complete MJPEG frames. Run calls emit for each frame until
// emit returns false or an error occurs. The frame slice is only valid during
// the call; dropped is the number of frames lost since the previous one.
// While no frames arrive, Run calls emit(nil, 0) about every 500ms so the
// caller can still stop it.
type Source interface {
	Run(emit func(frame []byte, dropped int) bool) error
	Close()
}

func main() {
	backend := flag.String("backend", "v4l2", "capture backend: v4l2, or usbfs (LicheeRV Nano workaround for slow uvcvideo)")
	dev := flag.String("dev", "/dev/video0", "V4L2 device (v4l2 backend)")
	usbDev := flag.String("usbdev", "/sys/bus/usb/devices/1-1", "sysfs path of the UVC device (usbfs backend)")
	size := flag.String("size", "1920x1080", "resolution WxH (initial resolution in serve mode)")
	sizes := flag.String("sizes", "1280x720,1920x1080", "serve mode: resolutions selectable from the web UI")
	fps := flag.Int("fps", 30, "frame rate")
	nbufs := flag.Int("bufs", 4, "number of V4L2 buffers (v4l2 backend)")
	alt := flag.Int("alt", 0, "isochronous alternate setting, 0 = as requested by device (usbfs backend)")
	nurbs := flag.Int("urbs", 8, "number of URBs in flight (usbfs backend)")
	npkts := flag.Int("pkts", 32, "isochronous packets per URB (usbfs backend)")
	dur := flag.Duration("t", 0, "test mode: capture duration (0 = serve HTTP forever)")
	out := flag.String("o", "", "test mode: write raw MJPEG stream to this file")
	listen := flag.String("listen", ":8081", "HTTP listen address (serve mode)")
	memLimit := flag.Int("memlimit", 24, "Go soft memory limit in MiB")
	audioDev := flag.String("audiodev", "hw:CARD=MS2109,DEV=0", "serve mode: ALSA capture device for HDMI audio (empty = disabled)")
	hidPath := flag.String("hid", "", "serve mode: serial port of the ESP32 HID bridge (empty = no keyboard/mouse input)")
	useSyslog := flag.Bool("syslog", false, "log to syslog instead of stderr")
	spin := flag.Duration("spin", 0, "run the idle-CPU probe for this long and exit")
	flag.Parse()
	if *spin > 0 {
		runSpin(*spin)
		return
	}
	if *useSyslog {
		w, err := syslog.New(syslog.LOG_INFO|syslog.LOG_DAEMON, "kvmcap")
		if err != nil {
			log.Fatal(err)
		}
		log.SetOutput(w)
		log.SetFlags(0)
	}

	debug.SetMemoryLimit(int64(*memLimit) << 20)

	open := func(size string) (Source, error) {
		var w, h int
		if _, err := fmt.Sscanf(size, "%dx%d", &w, &h); err != nil {
			return nil, fmt.Errorf("bad size %q", size)
		}
		switch *backend {
		case "v4l2":
			d, err := OpenDevice(*dev, w, h, *fps, *nbufs)
			if err != nil {
				return nil, err
			}
			log.Printf("v4l2: %dx%d MJPEG, interval=%d/%d, sizeimage=%d, buffers=%d x %d bytes",
				d.Width, d.Height, d.FrameIntervalNum, d.FrameIntervalDen, d.SizeImage, d.NumBuffers(), d.BufferLen())
			return d, nil
		case "usbfs":
			c, err := OpenUSB(*usbDev, w, h, *fps, *alt, *nurbs, *npkts)
			if err != nil {
				return nil, err
			}
			log.Printf("usbfs: %dx%d MJPEG, interval=%d00ns, alt=%d (%d B/uframe), device payload=%d, max frame=%d, urbs=%dx%d",
				c.Width, c.Height, c.Interval, c.Alt, c.pktSize, c.MaxPayload, c.MaxFrame, *nurbs, *npkts)
			return c, nil
		}
		return nil, fmt.Errorf("unknown -backend %q", *backend)
	}

	var stop atomic.Bool
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		stop.Store(true)
	}()

	if *dur == 0 {
		allowed := strings.Split(*sizes, ",")
		if !slices.Contains(allowed, *size) {
			log.Fatalf("-size %s is not in -sizes %s", *size, *sizes)
		}
		os.Exit(runServer(open, *size, allowed, *audioDev, *hidPath, *listen, &stop))
	}

	log.Printf("mem before open: %s", memInfo())
	src, err := open(*size)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("mem after open: %s", memInfo())
	code := runTest(src, *dur, *out, &stop)
	src.Close()
	os.Exit(code)
}

// runTest captures for dur and prints per-second statistics.
func runTest(src Source, dur time.Duration, outPath string, stop *atomic.Bool) int {
	var out *bufio.Writer
	if outPath != "" {
		f, err := os.Create(outPath)
		if err != nil {
			log.Print(err)
			return 1
		}
		defer f.Close()
		out = bufio.NewWriterSize(f, 256<<10)
		defer out.Flush()
	}

	start := time.Now()
	tick := start
	cpu0 := readCPU()
	secCPU := cpu0
	var (
		total, secFrames, drops, secDrops int
		totalBytes, secBytes, maxSize     int
		last                              time.Time
		maxGap, secMaxGap                 time.Duration
		minAvail                          = 1 << 30
		writeErr                          error
	)
	report := func(now time.Time) {
		avail := memAvailableKB()
		if avail < minAvail {
			minAvail = avail
		}
		el := now.Sub(tick).Seconds()
		c := readCPU()
		log.Printf("t=%5.1fs fps=%5.1f %6.2fMbps avg=%3dKB maxgap=%4dms drops=%d cpu=%3.0f%% memavail=%dKB",
			now.Sub(start).Seconds(), float64(secFrames)/el, float64(secBytes)*8/el/1e6,
			div(secBytes, secFrames)/1024, secMaxGap.Milliseconds(), secDrops, c.busyPct(secCPU), avail)
		secCPU = c
		secFrames, secBytes, secDrops, secMaxGap = 0, 0, 0, 0
		tick = now
	}

	err := src.Run(func(frame []byte, dropped int) bool {
		now := time.Now()
		if frame == nil {
			return !stop.Load() && now.Sub(start) < dur
		}
		n := len(frame)
		if out != nil {
			if _, writeErr = out.Write(frame); writeErr != nil {
				return false
			}
		}
		if !last.IsZero() {
			g := now.Sub(last)
			maxGap = max(maxGap, g)
			secMaxGap = max(secMaxGap, g)
		}
		last = now
		drops += dropped
		secDrops += dropped
		total++
		secFrames++
		totalBytes += n
		secBytes += n
		maxSize = max(maxSize, n)
		if now.Sub(tick) >= time.Second {
			report(now)
		}
		return !stop.Load() && now.Sub(start) < dur
	})
	if err == nil {
		err = writeErr
	}
	if err != nil {
		log.Print(err)
	}
	el := time.Since(start).Seconds()
	log.Printf("SUMMARY frames=%d (%.2f fps) drops=%d maxgap=%dms avg=%dKB max=%dKB %.2fMbps cpu=%.0f%% min_memavail=%dKB",
		total, float64(total)/el, drops, maxGap.Milliseconds(), div(totalBytes, total)/1024, maxSize/1024,
		float64(totalBytes)*8/el/1e6, readCPU().busyPct(cpu0), minAvail)
	if c, ok := src.(*USBCapture); ok {
		log.Printf("usbfs: packet errors=%d bad frames=%d", c.PktErrors, c.BadFrames)
	}
	if err != nil {
		return 1
	}
	return 0
}

func div(a, b int) int {
	if b == 0 {
		return 0
	}
	return a / b
}

func readMeminfo() map[string]int {
	m := map[string]int{}
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(v), " kB"))
		m[k] = n
	}
	return m
}

func memAvailableKB() int { return readMeminfo()["MemAvailable"] }

func memInfo() string {
	m := readMeminfo()
	return fmt.Sprintf("MemFree=%dKB MemAvailable=%dKB", m["MemFree"], m["MemAvailable"])
}

type cpuStat struct{ busy, total uint64 }

// readCPU returns aggregate jiffies from the "cpu" line of /proc/stat.
// Note: on this board the tick-based accounting aliases with the USB
// interrupt rate and is unreliable; use -spin for real idle measurements.
func readCPU() cpuStat {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuStat{}
	}
	line, _, _ := strings.Cut(string(data), "\n")
	var c cpuStat
	for i, f := range strings.Fields(line)[1:] {
		n, _ := strconv.ParseUint(f, 10, 64)
		c.total += n
		if i != 3 && i != 4 { // idle, iowait
			c.busy += n
		}
	}
	return c
}

func (c cpuStat) busyPct(prev cpuStat) float64 {
	if c.total == prev.total {
		return 0
	}
	return float64(c.busy-prev.busy) * 100 / float64(c.total-prev.total)
}
