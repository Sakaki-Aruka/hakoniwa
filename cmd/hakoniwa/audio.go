package main

// HDMI audio from the capture board, streamed to browsers as raw PCM
// (S16LE, 48 kHz, stereo, ~1.5 Mbps) over a WebSocket.
//
// Capture runs `arecord` only while at least one client is listening.

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

const (
	audioRate     = 48000
	audioChannels = 2
	audioChunk    = audioRate / 50 * audioChannels * 2 // 20 ms of S16LE stereo = 3840 bytes
	audioQueue    = 5                                   // per-client backlog: 100 ms
	audioIdleStop = 3 * time.Second
)

type audioHub struct {
	device string

	mu      sync.Mutex
	subs    map[chan []byte]struct{}
	wake    chan struct{}
	running bool
	lastErr string

	chunks atomic.Uint64
	drops  atomic.Uint64 // chunks dropped because a client fell behind
}

func newAudioHub(device string) *audioHub {
	return &audioHub{device: device, subs: map[chan []byte]struct{}{}, wake: make(chan struct{}, 1)}
}

func (a *audioHub) subscribe() chan []byte {
	ch := make(chan []byte, audioQueue)
	a.mu.Lock()
	a.subs[ch] = struct{}{}
	a.mu.Unlock()
	select {
	case a.wake <- struct{}{}:
	default:
	}
	return ch
}

func (a *audioHub) unsubscribe(ch chan []byte) {
	a.mu.Lock()
	delete(a.subs, ch)
	a.mu.Unlock()
}

func (a *audioHub) listeners() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.subs)
}

func (a *audioHub) status() (running bool, listeners int, lastErr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running, len(a.subs), a.lastErr
}

func (a *audioHub) broadcast(b []byte) {
	a.chunks.Add(1)
	a.mu.Lock()
	defer a.mu.Unlock()
	for ch := range a.subs {
		select {
		case ch <- b:
		default:
			// Client fell behind: drop its oldest chunk to keep latency low.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- b:
			default:
			}
			a.drops.Add(1)
		}
	}
}

// loop starts arecord when someone listens and stops it after the last
// listener has been gone for audioIdleStop.
func (a *audioHub) loop(stop *atomic.Bool) {
	for !stop.Load() {
		if a.listeners() == 0 {
			select {
			case <-a.wake:
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		err := a.capture(stop)
		a.mu.Lock()
		a.running = false
		if err != nil {
			a.lastErr = err.Error()
		}
		a.mu.Unlock()
		if err != nil {
			log.Printf("audio: %v", err)
			time.Sleep(2 * time.Second)
		}
	}
}

func (a *audioHub) capture(stop *atomic.Bool) error {
	cmd := exec.Command("arecord", "-q", "-D", a.device, "-t", "raw",
		"-f", "S16_LE", "-c", "2", "-r", "48000",
		"--period-time=10000", "--buffer-time=100000")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := &tailWriter{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	log.Printf("audio: capture started (%s)", a.device)
	a.mu.Lock()
	a.running = true
	a.lastErr = ""
	a.mu.Unlock()

	// Stop arecord once nobody has listened for a while (or on shutdown).
	done := make(chan struct{})
	go func() {
		idleSince := time.Time{}
		for {
			select {
			case <-done:
				return
			case <-time.After(250 * time.Millisecond):
			}
			if stop.Load() {
				cmd.Process.Kill()
				return
			}
			if a.listeners() > 0 {
				idleSince = time.Time{}
			} else if idleSince.IsZero() {
				idleSince = time.Now()
			} else if time.Since(idleSince) > audioIdleStop {
				cmd.Process.Kill()
				return
			}
		}
	}()

	r := bufio.NewReaderSize(out, audioChunk*4)
	var readErr error
	for {
		b := make([]byte, audioChunk)
		if _, readErr = io.ReadFull(r, b); readErr != nil {
			break
		}
		a.broadcast(b)
	}
	close(done)
	cmd.Process.Kill()
	waitErr := cmd.Wait()
	log.Printf("audio: capture stopped")
	if a.listeners() == 0 || stop.Load() {
		return nil // stopped on purpose
	}
	if msg := stderr.String(); msg != "" {
		return &audioError{msg}
	}
	if waitErr != nil {
		return waitErr
	}
	return readErr
}

type audioError struct{ msg string }

func (e *audioError) Error() string { return "arecord: " + e.msg }

// tailWriter keeps the last few hundred bytes written (arecord's stderr).
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 512 {
		t.buf = t.buf[len(t.buf)-512:]
	}
	return len(p), nil
}

func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// serveAudioWS streams PCM chunks as binary messages. The first message is
// a JSON text message describing the format.
func (a *audioHub) serveWS(w http.ResponseWriter, r *http.Request) {
	ws, err := wsUpgrade(w, r)
	if err != nil {
		return
	}
	defer ws.Close()

	hdr, _ := json.Marshal(map[string]any{"type": "format", "format": "s16le", "rate": audioRate, "channels": audioChannels})
	if err := ws.WriteMessage(wsOpText, hdr); err != nil {
		return
	}

	ch := a.subscribe()
	defer a.unsubscribe(ch)

	// Read side: only needed to notice the client going away.
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case b := <-ch:
			if err := ws.WriteMessage(wsOpBinary, b); err != nil {
				return
			}
		case <-gone:
			return
		}
	}
}
