package main

// Raw serial port access via termios (no cgo).

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// cbaud is CBAUD from <asm-generic/termbits.h>; the syscall package lacks it.
const cbaud = 0x100f

// openSerial opens a tty in raw 8N1 mode at 115200 baud. The fd is
// non-blocking so the returned *os.File uses the Go poller and Close
// unblocks a pending Read.
func openSerial(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	var t syscall.Termios
	if err := ioctl(fd, syscall.TCGETS, unsafe.Pointer(&t)); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("TCGETS: %w", err)
	}
	// cfmakeraw
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR |
		syscall.IGNCR | syscall.ICRNL | syscall.IXON | syscall.IXOFF
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB | cbaud | syscall.HUPCL
	t.Cflag |= syscall.CS8 | syscall.CREAD | syscall.CLOCAL | syscall.B115200
	t.Ispeed = syscall.B115200
	t.Ospeed = syscall.B115200
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	if err := ioctl(fd, syscall.TCSETS, unsafe.Pointer(&t)); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("TCSETS: %w", err)
	}
	return os.NewFile(uintptr(fd), path), nil
}
