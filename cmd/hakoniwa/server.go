package main

import (
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// hub holds the latest frame and wakes waiting clients when a new one arrives.
// A frame buffer is an 8-byte header (capture time, float64 ms of serverMs,
// little endian) followed by the JPEG; see frameJPEG.
type hub struct {
	mu      sync.Mutex
	frame   []byte
	seq     uint64
	notify  chan struct{}
	clients atomic.Int32

	frames atomic.Uint64
	drops  atomic.Uint64
}

func newHub() *hub { return &hub{notify: make(chan struct{})} }

func (h *hub) publish(b []byte) {
	h.mu.Lock()
	h.frame = b
	h.seq++
	close(h.notify)
	h.notify = make(chan struct{})
	h.mu.Unlock()
}

// latest returns the current frame, its sequence and a channel closed on the next publish.
func (h *hub) latest() ([]byte, uint64, <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.frame, h.seq, h.notify
}

// capturer runs the capture source and reopens it when the requested
// resolution changes or the source fails.
type capturer struct {
	open    func(size string) (Source, error)
	hub     *hub
	stop    *atomic.Bool
	changed atomic.Bool

	mu   sync.Mutex
	want string
	cur  string // "" while not streaming
	err  string // last open/capture error
}

func (c *capturer) setSize(size string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if size != c.want {
		c.want = size
		c.changed.Store(true)
	}
}

func (c *capturer) status() (want, cur, err string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.want, c.cur, c.err
}

func (c *capturer) setState(cur string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur = cur
	if err != nil {
		c.err = err.Error()
	} else if cur != "" {
		c.err = ""
	}
}

func (c *capturer) loop() {
	// Capture must not starve behind HTTP/WebSocket senders: a late URB reap
	// loses video data, whereas a late send only delays a frame for a client.
	runtime.LockOSThread()
	if err := syscall.Setpriority(syscall.PRIO_PROCESS, syscall.Gettid(), -10); err != nil {
		log.Printf("setpriority: %v", err)
	}
	for !c.stop.Load() {
		c.mu.Lock()
		size := c.want
		c.changed.Store(false)
		c.mu.Unlock()

		src, err := c.open(size)
		if err != nil {
			log.Printf("open %s: %v", size, err)
			c.setState("", err)
			c.sleep(2 * time.Second)
			continue
		}
		c.setState(size, nil)
		err = src.Run(func(frame []byte, dropped int) bool {
			if frame != nil {
				// Copy out so the capture buffer can be reused immediately.
				b := make([]byte, frameHeader+len(frame))
				binary.LittleEndian.PutUint64(b, math.Float64bits(serverMs()))
				copy(b[frameHeader:], frame)
				c.hub.drops.Add(uint64(dropped))
				c.hub.frames.Add(1)
				c.hub.publish(b)
			}
			return !c.stop.Load() && !c.changed.Load()
		})
		src.Close()
		c.setState("", err)
		if err != nil {
			log.Printf("capture %s: %v", size, err)
			c.sleep(time.Second)
		}
	}
}

// sleep waits for d unless stopped or the size changes first.
func (c *capturer) sleep(d time.Duration) {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if c.stop.Load() || c.changed.Load() {
			return
		}
	}
}

const boundary = "kvmframe"

func runServer(open func(string) (Source, error), size string, sizes []string, audioDev, hidPath string, ln net.Listener, stop *atomic.Bool) int {
	h := newHub()
	c := &capturer{open: open, hub: h, stop: stop, want: size}
	capDone := make(chan struct{})
	go func() {
		c.loop()
		close(capDone)
	}()
	var audio *audioHub
	if audioDev != "" {
		audio = newAudioHub(audioDev)
		go audio.loop(stop)
	}
	areaDet := newAreaDetector()
	go areaDet.run(h, 2*time.Second)
	var hid *hidBridge
	if hidPath != "" {
		hid = newHIDBridge(hidPath)
		go hid.loop(stop)
	}

	mux := http.NewServeMux()
	registerLocaleRoutes(mux)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, indexHTML)
	})
	mux.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		h.clients.Add(1)
		defer h.clients.Add(-1)
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
		w.Header().Set("Cache-Control", "no-store")
		fl, _ := w.(http.Flusher)
		// The boundary follows each frame immediately so the browser can show
		// it without waiting for the next one.
		if _, err := fmt.Fprintf(w, "--%s\r\n", boundary); err != nil {
			return
		}
		var sent uint64
		for {
			b, seq, ch := h.latest()
			if seq == sent || b == nil {
				select {
				case <-ch:
					continue
				case <-r.Context().Done():
					return
				}
			}
			sent = seq
			jpeg := frameJPEG(b)
			_, err := fmt.Fprintf(w, "Content-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(jpeg))
			if err == nil {
				_, err = w.Write(jpeg)
			}
			if err == nil {
				_, err = fmt.Fprintf(w, "\r\n--%s\r\n", boundary)
			}
			if err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
	})
	mux.HandleFunc("GET /snapshot.jpg", func(w http.ResponseWriter, r *http.Request) {
		b, _, ch := h.latest()
		if b == nil {
			select {
			case <-ch:
				b, _, _ = h.latest()
			case <-time.After(3 * time.Second):
				http.Error(w, "no frame", http.StatusServiceUnavailable)
				return
			}
		}
		jpeg := frameJPEG(b)
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Content-Length", strconv.Itoa(len(jpeg)))
		w.Write(jpeg)
	})
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "frames=%d drops=%d clients=%d %s\n", h.frames.Load(), h.drops.Load(), h.clients.Load(), memInfo())
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		want, cur, errStr := c.status()
		st := map[string]any{
			"size":    want,
			"active":  cur,
			"error":   errStr,
			"sizes":   sizes,
			"frames":  h.frames.Load(),
			"drops":   h.drops.Load(),
			"clients": h.clients.Load(),
			"audio":   nil,
			"area":    areaDet.get(),
		}
		if audio != nil {
			running, listeners, aerr := audio.status()
			st["audio"] = map[string]any{
				"running":   running,
				"listeners": listeners,
				"error":     aerr,
				"chunks":    audio.chunks.Load(),
				"drops":     audio.drops.Load(),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("GET /ws/video", func(w http.ResponseWriter, r *http.Request) {
		serveVideoWS(h, w, r)
	})
	mux.HandleFunc("GET /ws/input", func(w http.ResponseWriter, r *http.Request) {
		if hid == nil {
			http.Error(w, "input disabled", http.StatusNotFound)
			return
		}
		hid.serveWS(w, r)
	})
	mux.HandleFunc("GET /ws/audio", func(w http.ResponseWriter, r *http.Request) {
		if audio == nil {
			http.Error(w, "audio disabled", http.StatusNotFound)
			return
		}
		audio.serveWS(w, r)
	})
	mux.HandleFunc("POST /api/size", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		size := r.FormValue("size")
		if !slices.Contains(sizes, size) {
			http.Error(w, "unsupported size", http.StatusBadRequest)
			return
		}
		log.Printf("size change requested: %s", size)
		c.setSize(size)
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{Handler: mux}
	go func() {
		<-capDone
		srv.Close()
	}()

	go func() {
		var last uint64
		for range time.Tick(60 * time.Second) {
			n := h.frames.Load()
			_, cur, _ := c.status()
			log.Printf("size=%s fps=%.1f drops=%d clients=%d %s", cur, float64(n-last)/60, h.drops.Load(), h.clients.Load(), memInfo())
			last = n
		}
	}()

	log.Printf("serving on http://%s/", ln.Addr())
	err := srv.Serve(ln)
	stop.Store(true)
	<-capDone
	if hid != nil {
		// WebSocket handlers are not closed by srv.Close, so make sure
		// nothing stays pressed on the target when the session ends.
		hid.send("KR")
		hid.send("MB 0")
	}
	if err != nil && err != http.ErrServerClosed {
		log.Print(err)
		return 1
	}
	log.Printf("session stopped")
	return 0
}

//go:embed index.html
var indexHTML string
