package main

// User-space UVC (MJPEG) capture over usbfs.
//
// The in-kernel uvcvideo of this 5.10 kernel copies every isochronous packet
// out of uncached (coherent DMA) URB buffers, which costs ~1.7% CPU per Mbps
// on the C906 and caps capture at ~45 Mbps. usbfs allocates URB buffers with
// kmalloc (cached, streaming DMA), so the copy to user space is cheap.
//
// The uvcvideo driver is unbound from the device for the duration of the
// capture and rebound on Close.

import (
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

const (
	usbdevfsControl          = 0xc0185500
	usbdevfsSetInterface     = 0x80085504
	usbdevfsSubmitURB        = 0x8038550a
	usbdevfsDiscardURB       = 0x0000550b
	usbdevfsReapURB          = 0x4008550c
	usbdevfsReapURBNDelay    = 0x4008550d
	usbdevfsClaimInterface   = 0x8004550f
	usbdevfsReleaseInterface = 0x80045510

	urbTypeIso    = 0
	urbFlagIsoASAP = 0x02

	urbHeaderSize = 56 // sizeof(struct usbdevfs_urb) on 64-bit
	isoDescSize   = 12 // sizeof(struct usbdevfs_iso_packet_desc)

	uvcSetCur = 0x01
	uvcGetCur = 0x81
	uvcProbe  = 0x01
	uvcCommit = 0x02
)

type usbCtrl struct {
	RequestType uint8
	Request     uint8
	Value       uint16
	Index       uint16
	Length      uint16
	Timeout     uint32
	_           uint32
	Data        uintptr
}

type usbSetIntf struct {
	Interface  uint32
	AltSetting uint32
}

type uvcFrameDesc struct {
	formatIndex, frameIndex int
	width, height          int
	intervals              []uint32 // 100ns units
}

type isoAlt struct {
	alt       int
	ep        uint8
	bytesPerU int // maxpacket * mult per microframe
}

type usbDescs struct {
	vsIntf int
	frames []uvcFrameDesc
	alts   []isoAlt
}

// parseConfig extracts the MJPEG frame descriptors and the isochronous
// alternate settings of the video streaming interface.
func parseConfig(d []byte) (*usbDescs, error) {
	r := &usbDescs{vsIntf: -1}
	curIntf, curAlt, curClass, curSub := -1, -1, 0, 0
	mjpegFormat := -1
	for i := 0; i+2 <= len(d); {
		l, t := int(d[i]), d[i+1]
		if l < 2 || i+l > len(d) {
			break
		}
		b := d[i : i+l]
		switch {
		case t == 4 && l >= 9: // interface
			curIntf, curAlt, curClass, curSub = int(b[2]), int(b[3]), int(b[5]), int(b[6])
			if curClass == 0x0e && curSub == 0x02 && r.vsIntf < 0 {
				r.vsIntf = curIntf
			}
		case t == 5 && l >= 7 && curIntf == r.vsIntf && curAlt > 0: // endpoint
			if b[3]&3 == 1 && b[2]&0x80 != 0 {
				mps := int(binary.LittleEndian.Uint16(b[4:]))
				r.alts = append(r.alts, isoAlt{alt: curAlt, ep: b[2], bytesPerU: (mps & 0x7ff) * ((mps>>11)&3 + 1)})
			}
		case t == 0x24 && curClass == 0x0e && curSub == 0x02 && l >= 3: // CS_INTERFACE (VS)
			switch b[2] {
			case 0x06: // VS_FORMAT_MJPEG
				mjpegFormat = int(b[3])
			case 0x07: // VS_FRAME_MJPEG
				if mjpegFormat < 0 || l < 26 {
					break
				}
				f := uvcFrameDesc{
					formatIndex: mjpegFormat,
					frameIndex:  int(b[3]),
					width:       int(binary.LittleEndian.Uint16(b[5:])),
					height:      int(binary.LittleEndian.Uint16(b[7:])),
				}
				n := int(b[25])
				if n == 0 {
					n = 3 // continuous: min, max, step
				}
				for k := 0; k < n && 26+4*k+4 <= l; k++ {
					f.intervals = append(f.intervals, binary.LittleEndian.Uint32(b[26+4*k:]))
				}
				r.frames = append(r.frames, f)
			case 0x04, 0x10: // uncompressed / frame-based format: stop tagging frames as MJPEG
				mjpegFormat = -1
			}
		}
		i += l
	}
	if r.vsIntf < 0 {
		return nil, fmt.Errorf("no video streaming interface")
	}
	return r, nil
}

type USBCapture struct {
	fd        int
	sysPath   string // /sys/bus/usb/devices/1-1
	unbound   []string
	vsIntf    int
	ep        uint8
	pktSize   int
	nPackets  int
	urbs      []uintptr
	urbMem    []byte
	bufMem    []byte
	Width     int
	Height    int
	Interval  uint32 // 100ns units
	Alt       int
	MaxFrame  int
	MaxPayload int

	// stats
	PktErrors  int
	BadFrames  int
}

// OpenUSB takes over the UVC device at sysDev (e.g. /sys/bus/usb/devices/1-1)
// and negotiates an MJPEG stream.
func OpenUSB(sysDev string, width, height, fps, alt, nURBs, nPackets int) (*USBCapture, error) {
	c := &USBCapture{fd: -1, sysPath: sysDev, nPackets: nPackets}
	ok := false
	defer func() {
		if !ok {
			c.Close()
		}
	}()

	busnum, err := readInt(filepath.Join(sysDev, "busnum"))
	if err != nil {
		return nil, err
	}
	devnum, err := readInt(filepath.Join(sysDev, "devnum"))
	if err != nil {
		return nil, err
	}
	desc, err := os.ReadFile(filepath.Join(sysDev, "descriptors"))
	if err != nil {
		return nil, err
	}
	if len(desc) < 18 {
		return nil, fmt.Errorf("short descriptors")
	}
	ds, err := parseConfig(desc[18:])
	if err != nil {
		return nil, err
	}
	c.vsIntf = ds.vsIntf

	var fr *uvcFrameDesc
	for i := range ds.frames {
		if ds.frames[i].width == width && ds.frames[i].height == height {
			fr = &ds.frames[i]
			break
		}
	}
	if fr == nil {
		return nil, fmt.Errorf("no MJPEG frame descriptor for %dx%d", width, height)
	}

	// Detach uvcvideo from all of its interfaces on this device.
	ents, _ := filepath.Glob(sysDev + "/" + filepath.Base(sysDev) + ":*")
	for _, e := range ents {
		drv, err := os.Readlink(filepath.Join(e, "driver"))
		if err != nil || filepath.Base(drv) != "uvcvideo" {
			continue
		}
		name := filepath.Base(e)
		if err := os.WriteFile("/sys/bus/usb/drivers/uvcvideo/unbind", []byte(name), 0); err != nil {
			return nil, fmt.Errorf("unbind %s: %w", name, err)
		}
		c.unbound = append(c.unbound, name)
	}

	path := fmt.Sprintf("/dev/bus/usb/%03d/%03d", busnum, devnum)
	c.fd, err = syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	intf := uint32(c.vsIntf)
	if err := ioctl(c.fd, usbdevfsClaimInterface, unsafe.Pointer(&intf)); err != nil {
		return nil, fmt.Errorf("claim interface %d: %w", c.vsIntf, err)
	}
	// Make sure streaming is stopped before negotiating.
	si := usbSetIntf{Interface: intf, AltSetting: 0}
	ioctl(c.fd, usbdevfsSetInterface, unsafe.Pointer(&si))

	// Pick the frame interval closest to the requested fps.
	want := uint32(10000000 / fps)
	interval := fr.intervals[0]
	for _, iv := range fr.intervals {
		if absDiff(iv, want) < absDiff(interval, want) {
			interval = iv
		}
	}

	probe := make([]byte, 26)
	binary.LittleEndian.PutUint16(probe[0:], 1) // bmHint: dwFrameInterval
	probe[2] = byte(fr.formatIndex)
	probe[3] = byte(fr.frameIndex)
	binary.LittleEndian.PutUint32(probe[4:], interval)
	if err := c.control(0x21, uvcSetCur, uvcProbe<<8, probe); err != nil {
		return nil, fmt.Errorf("probe SET_CUR: %w", err)
	}
	if err := c.control(0xa1, uvcGetCur, uvcProbe<<8, probe); err != nil {
		return nil, fmt.Errorf("probe GET_CUR: %w", err)
	}
	if err := c.control(0x21, uvcSetCur, uvcCommit<<8, probe); err != nil {
		return nil, fmt.Errorf("commit SET_CUR: %w", err)
	}
	c.Width, c.Height = fr.width, fr.height
	c.Interval = binary.LittleEndian.Uint32(probe[4:])
	c.MaxFrame = int(binary.LittleEndian.Uint32(probe[18:]))
	c.MaxPayload = int(binary.LittleEndian.Uint32(probe[22:]))

	// Choose the alternate setting: explicit, or the smallest that fits the
	// payload size the device asked for.
	var sel *isoAlt
	for i := range ds.alts {
		a := &ds.alts[i]
		if alt > 0 {
			if a.alt == alt {
				sel = a
			}
		} else if a.bytesPerU >= c.MaxPayload && (sel == nil || a.bytesPerU < sel.bytesPerU) {
			sel = a
		}
	}
	if sel == nil && alt <= 0 && len(ds.alts) > 0 {
		sel = &ds.alts[len(ds.alts)-1]
	}
	if sel == nil {
		return nil, fmt.Errorf("no usable isochronous alternate setting")
	}
	c.Alt, c.ep, c.pktSize = sel.alt, sel.ep, sel.bytesPerU
	si = usbSetIntf{Interface: intf, AltSetting: uint32(sel.alt)}
	if err := ioctl(c.fd, usbdevfsSetInterface, unsafe.Pointer(&si)); err != nil {
		return nil, fmt.Errorf("set alt %d: %w", sel.alt, err)
	}

	// URB structs and data buffers live in mmap'd memory so the Go GC never
	// sees (or moves) memory the kernel holds pointers to.
	urbSize := urbHeaderSize + isoDescSize*nPackets
	urbSize = (urbSize + 7) &^ 7
	c.urbMem, err = syscall.Mmap(-1, 0, urbSize*nURBs, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		return nil, err
	}
	c.bufMem, err = syscall.Mmap(-1, 0, c.pktSize*nPackets*nURBs, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		return nil, err
	}
	for i := 0; i < nURBs; i++ {
		c.urbs = append(c.urbs, uintptr(unsafe.Pointer(&c.urbMem[i*urbSize])))
	}
	ok = true
	return c, nil
}

func (c *USBCapture) control(reqType, req uint8, value uint16, data []byte) error {
	ct := usbCtrl{
		RequestType: reqType,
		Request:     req,
		Value:       value,
		Index:       uint16(c.vsIntf),
		Length:      uint16(len(data)),
		Timeout:     1000,
		Data:        uintptr(unsafe.Pointer(&data[0])),
	}
	return ioctl(c.fd, usbdevfsControl, unsafe.Pointer(&ct))
}

func (c *USBCapture) urbIndex(p uintptr) int {
	for i, u := range c.urbs {
		if u == p {
			return i
		}
	}
	return -1
}

func (c *USBCapture) submit(i int) error {
	p := c.urbs[i]
	hdr := unsafe.Slice((*byte)(unsafe.Pointer(p)), urbHeaderSize+isoDescSize*c.nPackets)
	clear(hdr)
	le := binary.LittleEndian
	hdr[0] = urbTypeIso
	hdr[1] = c.ep
	le.PutUint32(hdr[8:], urbFlagIsoASAP)
	buf := uintptr(unsafe.Pointer(&c.bufMem[i*c.pktSize*c.nPackets]))
	le.PutUint64(hdr[16:], uint64(buf))
	le.PutUint32(hdr[24:], uint32(c.pktSize*c.nPackets))
	le.PutUint32(hdr[36:], uint32(c.nPackets))
	le.PutUint64(hdr[48:], uint64(i))
	for k := 0; k < c.nPackets; k++ {
		le.PutUint32(hdr[urbHeaderSize+k*isoDescSize:], uint32(c.pktSize))
	}
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(c.fd), usbdevfsSubmitURB, p)
	if e != 0 {
		return fmt.Errorf("submit urb: %w", e)
	}
	return nil
}

// Run streams until an error occurs, calling emit with each complete JPEG.
// The slice passed to emit is only valid during the call.
func (c *USBCapture) Run(emit func(frame []byte, dropped int) bool) error {
	for i := range c.urbs {
		if err := c.submit(i); err != nil {
			return err
		}
	}
	frame := make([]byte, 0, 1<<20)
	fid := -1
	bad := false
	dropped := 0
	running := true
	synced := false // the first frame is joined mid-way; skip it silently
	le := binary.LittleEndian

	finish := func() {
		if !synced {
			synced = true
		} else if len(frame) > 0 {
			if bad || len(frame) < 4 || frame[0] != 0xff || frame[1] != 0xd8 {
				c.BadFrames++
				dropped++
			} else {
				running = emit(frame, dropped)
				dropped = 0
			}
		}
		frame = frame[:0]
		bad = false
	}

	for {
		var p uintptr
		_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(c.fd), usbdevfsReapURBNDelay, uintptr(unsafe.Pointer(&p)))
		if e == syscall.EINTR {
			continue
		}
		if e == syscall.EAGAIN {
			// usbfs signals completed URBs as POLLOUT.
			ready, err := pollFor(c.fd, pollOut, 500)
			if err != nil {
				return err
			}
			if !ready && !emit(nil, 0) {
				return nil
			}
			continue
		}
		if e != 0 {
			return fmt.Errorf("reap urb: %w", e)
		}
		i := c.urbIndex(p)
		if i < 0 {
			return fmt.Errorf("reaped unknown urb %#x", p)
		}
		hdr := unsafe.Slice((*byte)(unsafe.Pointer(p)), urbHeaderSize+isoDescSize*c.nPackets)
		if st := int32(le.Uint32(hdr[4:])); st != 0 && st != -int32(syscall.EXDEV) {
			return fmt.Errorf("urb status %d", st)
		}
		base := i * c.pktSize * c.nPackets
		for k := 0; k < c.nPackets; k++ {
			d := hdr[urbHeaderSize+k*isoDescSize:]
			alen := int(le.Uint32(d[4:]))
			if st := le.Uint32(d[8:]); st != 0 {
				c.PktErrors++
				bad = true
				continue
			}
			if alen < 2 {
				continue
			}
			// usbfs packs iso packets back to back at their *requested* length offsets.
			pkt := c.bufMem[base+k*c.pktSize : base+k*c.pktSize+alen]
			hl := int(pkt[0])
			if hl < 2 || hl > alen {
				bad = true
				continue
			}
			info := pkt[1]
			f := int(info & 1)
			if fid >= 0 && f != fid {
				finish() // FID toggled without EOF
			}
			fid = f
			if info&0x40 != 0 { // ERR
				bad = true
			}
			if len(frame)+alen-hl > c.MaxFrame && c.MaxFrame > 0 {
				bad = true
			} else {
				frame = append(frame, pkt[hl:]...)
			}
			if info&0x02 != 0 { // EOF
				finish()
				fid = -1
			}
		}
		if !running {
			return nil
		}
		if err := c.submit(i); err != nil {
			return err
		}
	}
}

func (c *USBCapture) Close() {
	if c.fd >= 0 {
		// Closing the fd kills outstanding URBs and releases the interface.
		si := usbSetIntf{Interface: uint32(c.vsIntf), AltSetting: 0}
		ioctl(c.fd, usbdevfsSetInterface, unsafe.Pointer(&si))
		intf := uint32(c.vsIntf)
		ioctl(c.fd, usbdevfsReleaseInterface, unsafe.Pointer(&intf))
		syscall.Close(c.fd)
		c.fd = -1
	}
	if c.urbMem != nil {
		syscall.Munmap(c.urbMem)
		c.urbMem = nil
	}
	if c.bufMem != nil {
		syscall.Munmap(c.bufMem)
		c.bufMem = nil
	}
	for _, name := range c.unbound {
		if err := os.WriteFile("/sys/bus/usb/drivers/uvcvideo/bind", []byte(name), 0); err != nil {
			log.Printf("rebind %s: %v", name, err)
		}
	}
	c.unbound = nil
}

func readInt(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var n int
	_, err = fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &n)
	return n, err
}

func absDiff(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}
