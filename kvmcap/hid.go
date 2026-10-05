package main

// Bridge to the ESP32-S3 HID firmware (esp32-hid) over its UART port, and
// the /ws/input WebSocket that browsers use to send key events.
//
// Browser → server (text JSON):
//   {"t":"kd","u":<usage>}   key down (HID Keyboard/Keypad page usage ID)
//   {"t":"ku","u":<usage>}   key up
//   {"t":"kr"}               release every key this client holds
//   {"t":"ma","x":n,"y":n}   absolute mouse move, 0..32767
//   {"t":"mr","x":n,"y":n}   relative mouse move
//   {"t":"mb","b":mask}      mouse buttons (bit0 left, bit1 right, bit2 middle, bit3 back, bit4 forward)
//   {"t":"mw","d":n}         wheel notches, positive = up
// Server → browser (text JSON):
//   {"t":"hid","connected":bool,"mounted":bool,"leds":n,"version":"..."}
//
// Keys and mouse buttons are tracked per client so a disconnect or a lost
// focus only releases that client's keys (mouse buttons are global on the
// target, so a client releases them only if it pressed any).

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type hidStatus struct {
	T         string `json:"t"`
	Connected bool   `json:"connected"` // serial port open and firmware answering
	Mounted   bool   `json:"mounted"`   // target PC enumerated the USB device
	LEDs      int    `json:"leds"`      // bit0 NumLock, bit1 CapsLock, bit2 ScrollLock
	Version   string `json:"version"`
	Error     string `json:"error,omitempty"`
}

type hidBridge struct {
	path string

	wmu sync.Mutex // serializes writes; guards f
	f   *os.File

	mu     sync.Mutex
	status hidStatus
	subs   map[chan hidStatus]struct{}

	lastReply atomic.Int64 // unix ms of the last line from the firmware
}

func newHIDBridge(path string) *hidBridge {
	return &hidBridge{path: path, status: hidStatus{T: "hid"}, subs: map[chan hidStatus]struct{}{}}
}

func (b *hidBridge) getStatus() hidStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status
}

func (b *hidBridge) update(fn func(s *hidStatus)) {
	b.mu.Lock()
	old := b.status
	fn(&b.status)
	s := b.status
	if s != old {
		for ch := range b.subs {
			select {
			case ch <- s:
			default:
			}
		}
	}
	b.mu.Unlock()
}

func (b *hidBridge) subscribe() chan hidStatus {
	ch := make(chan hidStatus, 8)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *hidBridge) unsubscribe(ch chan hidStatus) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}

// send writes one command line to the firmware.
func (b *hidBridge) send(cmd string) error {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	if b.f == nil {
		return fmt.Errorf("hid: not connected")
	}
	_, err := b.f.Write([]byte(cmd + "\n"))
	return err
}

// loop keeps the serial port open, reopening it after errors (e.g. unplug).
func (b *hidBridge) loop(stop *atomic.Bool) {
	for !stop.Load() {
		f, err := openSerial(b.path)
		if err != nil {
			b.update(func(s *hidStatus) { *s = hidStatus{T: "hid", Error: err.Error()} })
			time.Sleep(2 * time.Second)
			continue
		}
		log.Printf("hid: opened %s", b.path)
		b.wmu.Lock()
		b.f = f
		b.wmu.Unlock()

		done := make(chan struct{})
		go b.poll(done, stop)
		err = b.readLoop(f)
		close(done)

		b.wmu.Lock()
		b.f.Close()
		b.f = nil
		b.wmu.Unlock()
		b.update(func(s *hidStatus) { *s = hidStatus{T: "hid", Error: fmt.Sprint(err)} })
		if !stop.Load() {
			log.Printf("hid: %v", err)
			time.Sleep(time.Second)
		}
	}
}

// poll asks for the firmware status periodically and marks the bridge
// disconnected when the firmware stops answering.
func (b *hidBridge) poll(done chan struct{}, stop *atomic.Bool) {
	b.send("VER")
	for {
		b.send("ST")
		select {
		case <-done:
			return
		case <-time.After(2 * time.Second):
		}
		if stop.Load() {
			b.wmu.Lock()
			if b.f != nil {
				b.f.Close() // unblocks readLoop
			}
			b.wmu.Unlock()
			return
		}
		if time.Now().UnixMilli()-b.lastReply.Load() > 5000 {
			b.update(func(s *hidStatus) { s.Connected = false; s.Error = "no response from firmware" })
		}
	}
}

func (b *hidBridge) readLoop(f *os.File) error {
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		b.lastReply.Store(time.Now().UnixMilli())
		word, rest, _ := strings.Cut(line, " ")
		switch word {
		case "OK", "PONG":
		case "ERR":
			log.Printf("hid: firmware error: %s", rest)
		case "LED":
			if v, err := strconv.ParseUint(rest, 16, 8); err == nil {
				b.update(func(s *hidStatus) { s.LEDs = int(v) })
			}
		case "ST":
			b.update(func(s *hidStatus) {
				s.Connected, s.Error = true, ""
				for _, kv := range strings.Fields(rest) {
					k, v, _ := strings.Cut(kv, "=")
					switch k {
					case "mounted":
						s.Mounted = v == "1"
					case "leds":
						if n, err := strconv.ParseUint(v, 16, 8); err == nil {
							s.LEDs = int(n)
						}
					}
				}
			})
		case "VER":
			b.update(func(s *hidStatus) { s.Version = rest })
		case "READY":
			// Firmware (re)started: all keys are released on its side.
			log.Printf("hid: firmware started: %s", rest)
			b.update(func(s *hidStatus) { s.Version = rest })
			b.send("ST")
		default:
			// Boot ROM output, logs: ignore.
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return fmt.Errorf("serial port closed")
}

// sameOrigin rejects cross-site WebSocket/POST requests: without it any web
// page open in the user's browser could type on the target PC.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true // non-browser client
	}
	_, host, ok := strings.Cut(o, "://")
	return ok && strings.EqualFold(host, r.Host)
}

func (b *hidBridge) serveWS(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	ws, err := wsUpgrade(w, r)
	if err != nil {
		return
	}
	defer ws.Close()

	statusCh := b.subscribe()
	defer b.unsubscribe(statusCh)
	sendStatus := func(s hidStatus) error {
		msg, _ := json.Marshal(s)
		return ws.WriteMessage(wsOpText, msg)
	}
	if sendStatus(b.getStatus()) != nil {
		return
	}
	go func() {
		for s := range statusCh {
			if sendStatus(s) != nil {
				return
			}
		}
	}()

	held := map[uint8]bool{}
	release := func() {
		for u := range held {
			b.send(fmt.Sprintf("KU %#x", u))
		}
		clear(held)
	}
	buttons := 0
	defer func() {
		release()
		if buttons != 0 {
			b.send("MB 0")
		}
	}()

	for {
		op, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if op != wsOpText {
			continue
		}
		var m struct {
			T string `json:"t"`
			U int    `json:"u"`
			X int    `json:"x"`
			Y int    `json:"y"`
			B int    `json:"b"`
			D int    `json:"d"`
		}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.T {
		case "kd", "ku":
			if m.U <= 0 || m.U > 0xff {
				continue
			}
			u := uint8(m.U)
			if m.T == "kd" && !held[u] {
				held[u] = true
				b.send(fmt.Sprintf("KD %#x", u))
			} else if m.T == "ku" && held[u] {
				delete(held, u)
				b.send(fmt.Sprintf("KU %#x", u))
			}
		case "kr":
			release()
		case "ma":
			b.send(fmt.Sprintf("MA %d %d", clampInt(m.X, 0, 32767), clampInt(m.Y, 0, 32767)))
		case "mr":
			if m.X != 0 || m.Y != 0 {
				b.send(fmt.Sprintf("MR %d %d", clampInt(m.X, -4096, 4096), clampInt(m.Y, -4096, 4096)))
			}
		case "mb":
			buttons = m.B & 0x1f
			b.send(fmt.Sprintf("MB %d", buttons))
		case "mw":
			if m.D != 0 {
				b.send(fmt.Sprintf("MW %d", clampInt(m.D, -127, 127)))
			}
		}
	}
}

func clampInt(v, lo, hi int) int { return max(lo, min(hi, v)) }
