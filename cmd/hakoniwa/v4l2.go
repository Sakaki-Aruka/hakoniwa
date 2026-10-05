package main

// Minimal V4L2 MJPEG capture using raw ioctls (no cgo).
// Struct layouts are for 64-bit Linux (riscv64 / amd64).

import (
	"encoding/binary"
	"fmt"
	"syscall"
	"unsafe"
)

const (
	bufTypeVideoCapture = 1
	memoryMmap          = 1
	fieldAny            = 0
	pixFmtMJPEG         = 0x47504a4d // 'MJPG'

	vidiocGFmt     = 0xc0d05604
	vidiocSFmt     = 0xc0d05605
	vidiocReqbufs  = 0xc0145608
	vidiocQuerybuf = 0xc0585609
	vidiocQbuf     = 0xc058560f
	vidiocDqbuf    = 0xc0585611
	vidiocStreamon = 0x40045612
	vidiocStreamof = 0x40045613
	vidiocGParm    = 0xc0cc5615
	vidiocSParm    = 0xc0cc5616
)

// struct v4l2_format (208 bytes): type u32, pad, union fmt[200] at offset 8.
type v4l2Format struct {
	Type uint32
	_    uint32
	Fmt  [200]byte
}

// struct v4l2_requestbuffers (20 bytes)
type v4l2RequestBuffers struct {
	Count        uint32
	Type         uint32
	Memory       uint32
	Capabilities uint32
	Reserved     uint32
}

// struct v4l2_buffer (88 bytes on 64-bit)
type v4l2Buffer struct {
	Index     uint32
	Type      uint32
	BytesUsed uint32
	Flags     uint32
	Field     uint32
	_         uint32
	TvSec     int64
	TvUsec    int64
	Timecode  [16]byte
	Sequence  uint32
	Memory    uint32
	Offset    uint64 // union m (offset / userptr / planes / fd)
	Length    uint32
	Reserved2 uint32
	RequestFd uint32
	_         uint32
}

// struct v4l2_streamparm (204 bytes)
type v4l2StreamParm struct {
	Type uint32
	Parm [200]byte
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	for {
		_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
		if e == syscall.EINTR {
			continue
		}
		if e != 0 {
			return e
		}
		return nil
	}
}

type Device struct {
	fd        int
	bufs      [][]byte
	Width     int
	Height    int
	SizeImage int
	// Frame interval actually set by the driver (seconds = Num/Den).
	FrameIntervalNum, FrameIntervalDen uint32
	streaming bool
}

type Frame struct {
	Index    uint32
	Data     []byte // points into mmap buffer; valid until Release
	Sequence uint32
	TsUsec   int64
}

func OpenDevice(path string, width, height, fps, nbufs int) (*Device, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	d := &Device{fd: fd}
	ok := false
	defer func() {
		if !ok {
			d.Close()
		}
	}()

	// S_FMT
	var f v4l2Format
	f.Type = bufTypeVideoCapture
	le := binary.LittleEndian
	le.PutUint32(f.Fmt[0:], uint32(width))
	le.PutUint32(f.Fmt[4:], uint32(height))
	le.PutUint32(f.Fmt[8:], pixFmtMJPEG)
	le.PutUint32(f.Fmt[12:], fieldAny)
	if err := ioctl(fd, vidiocSFmt, unsafe.Pointer(&f)); err != nil {
		return nil, fmt.Errorf("VIDIOC_S_FMT: %w", err)
	}
	d.Width = int(le.Uint32(f.Fmt[0:]))
	d.Height = int(le.Uint32(f.Fmt[4:]))
	if pf := le.Uint32(f.Fmt[8:]); pf != pixFmtMJPEG {
		return nil, fmt.Errorf("device did not accept MJPEG (got %08x)", pf)
	}
	d.SizeImage = int(le.Uint32(f.Fmt[20:]))

	// S_PARM (frame rate)
	if fps > 0 {
		var p v4l2StreamParm
		p.Type = bufTypeVideoCapture
		if err := ioctl(fd, vidiocGParm, unsafe.Pointer(&p)); err != nil {
			return nil, fmt.Errorf("VIDIOC_G_PARM: %w", err)
		}
		// capture parm: capability, capturemode, timeperframe{num, den}, ...
		le.PutUint32(p.Parm[8:], 1)
		le.PutUint32(p.Parm[12:], uint32(fps))
		if err := ioctl(fd, vidiocSParm, unsafe.Pointer(&p)); err != nil {
			return nil, fmt.Errorf("VIDIOC_S_PARM: %w", err)
		}
		d.FrameIntervalNum = le.Uint32(p.Parm[8:])
		d.FrameIntervalDen = le.Uint32(p.Parm[12:])
	}

	// REQBUFS
	rb := v4l2RequestBuffers{Count: uint32(nbufs), Type: bufTypeVideoCapture, Memory: memoryMmap}
	if err := ioctl(fd, vidiocReqbufs, unsafe.Pointer(&rb)); err != nil {
		return nil, fmt.Errorf("VIDIOC_REQBUFS: %w", err)
	}
	if rb.Count == 0 {
		return nil, fmt.Errorf("VIDIOC_REQBUFS: driver returned 0 buffers")
	}

	for i := uint32(0); i < rb.Count; i++ {
		b := v4l2Buffer{Index: i, Type: bufTypeVideoCapture, Memory: memoryMmap}
		if err := ioctl(fd, vidiocQuerybuf, unsafe.Pointer(&b)); err != nil {
			return nil, fmt.Errorf("VIDIOC_QUERYBUF %d: %w", i, err)
		}
		m, err := syscall.Mmap(fd, int64(b.Offset), int(b.Length), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			return nil, fmt.Errorf("mmap buffer %d: %w", i, err)
		}
		d.bufs = append(d.bufs, m)
	}
	ok = true
	return d, nil
}

func (d *Device) NumBuffers() int { return len(d.bufs) }
func (d *Device) BufferLen() int {
	if len(d.bufs) == 0 {
		return 0
	}
	return len(d.bufs[0])
}

func (d *Device) Start() error {
	for i := range d.bufs {
		if err := d.qbuf(uint32(i)); err != nil {
			return err
		}
	}
	t := uint32(bufTypeVideoCapture)
	if err := ioctl(d.fd, vidiocStreamon, unsafe.Pointer(&t)); err != nil {
		return fmt.Errorf("VIDIOC_STREAMON: %w", err)
	}
	d.streaming = true
	return nil
}

func (d *Device) qbuf(i uint32) error {
	b := v4l2Buffer{Index: i, Type: bufTypeVideoCapture, Memory: memoryMmap}
	if err := ioctl(d.fd, vidiocQbuf, unsafe.Pointer(&b)); err != nil {
		return fmt.Errorf("VIDIOC_QBUF %d: %w", i, err)
	}
	return nil
}

// Next waits up to timeoutMs for a frame. Returns (nil, nil) on timeout.
func (d *Device) Next(timeoutMs int) (*Frame, error) {
	for {
		b := v4l2Buffer{Type: bufTypeVideoCapture, Memory: memoryMmap}
		err := ioctl(d.fd, vidiocDqbuf, unsafe.Pointer(&b))
		if err == nil {
			return &Frame{
				Index:    b.Index,
				Data:     d.bufs[b.Index][:b.BytesUsed],
				Sequence: b.Sequence,
				TsUsec:   b.TvSec*1e6 + b.TvUsec,
			}, nil
		}
		if err != syscall.EAGAIN {
			return nil, fmt.Errorf("VIDIOC_DQBUF: %w", err)
		}
		ready, err := pollFor(d.fd, pollIn, timeoutMs)
		if err != nil {
			return nil, err
		}
		if !ready {
			return nil, nil
		}
	}
}

func (d *Device) Release(f *Frame) error { return d.qbuf(f.Index) }

// Run implements Source.
func (d *Device) Run(emit func(frame []byte, dropped int) bool) error {
	if !d.streaming {
		if err := d.Start(); err != nil {
			return err
		}
	}
	var lastSeq uint32
	have := false
	for {
		f, err := d.Next(500)
		if err != nil {
			return err
		}
		if f == nil {
			if !emit(nil, 0) {
				return nil
			}
			continue
		}
		dropped := 0
		if have {
			if gap := f.Sequence - lastSeq - 1; gap > 0 && gap < 1<<31 {
				dropped = int(gap)
			}
		}
		lastSeq, have = f.Sequence, true
		more := emit(f.Data, dropped)
		if err := d.Release(f); err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
}

func (d *Device) Close() {
	if d.streaming {
		t := uint32(bufTypeVideoCapture)
		ioctl(d.fd, vidiocStreamof, unsafe.Pointer(&t))
		d.streaming = false
	}
	for _, m := range d.bufs {
		syscall.Munmap(m)
	}
	d.bufs = nil
	if d.fd >= 0 {
		// Free driver buffers before close.
		rb := v4l2RequestBuffers{Count: 0, Type: bufTypeVideoCapture, Memory: memoryMmap}
		ioctl(d.fd, vidiocReqbufs, unsafe.Pointer(&rb))
		syscall.Close(d.fd)
		d.fd = -1
	}
}

type pollFd struct {
	Fd      int32
	Events  int16
	Revents int16
}

const (
	pollIn  = 0x1
	pollOut = 0x4
)

func pollFor(fd int, events int16, timeoutMs int) (bool, error) {
	p := pollFd{Fd: int32(fd), Events: events}
	for {
		// riscv64 has no SYS_POLL; use ppoll.
		var ts *syscall.Timespec
		if timeoutMs >= 0 {
			t := syscall.NsecToTimespec(int64(timeoutMs) * 1e6)
			ts = &t
		}
		n, _, e := syscall.Syscall6(syscall.SYS_PPOLL, uintptr(unsafe.Pointer(&p)), 1, uintptr(unsafe.Pointer(ts)), 0, 0, 0)
		if e == syscall.EINTR {
			continue
		}
		if e != 0 {
			return false, fmt.Errorf("ppoll: %w", e)
		}
		if p.Revents&(0x8|0x10|0x20) != 0 { // POLLERR|POLLHUP|POLLNVAL
			return false, fmt.Errorf("ppoll: device error (revents=%#x)", p.Revents)
		}
		return n > 0, nil
	}
}
