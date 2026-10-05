package main

// Video over WebSocket with client-driven flow control.
//
// multipart/x-mixed-replace has no back-pressure: when the browser renders
// slower than frames arrive, they pile up in the TCP send/receive buffers
// (megabytes, i.e. many frames) and latency grows. Here the client acks
// every frame after drawing it and the server keeps at most videoInflight
// frames unacknowledged, always sending the newest frame.
//
// Server → client binary: [8-byte capture time, float64 ms LE][JPEG]
// Client → server text:   {"t":"ack"}  |  {"t":"ping","c":<client ms>}
// Server → client text:   {"t":"pong","c":<echo>,"s":<server ms>}

import (
	"encoding/json"
	"net/http"
	"time"
)

const (
	frameHeader   = 8
	videoInflight = 2
)

var serverStart = time.Now()

// serverMs is the server clock shared with clients for latency measurement.
func serverMs() float64 { return float64(time.Since(serverStart).Microseconds()) / 1000 }

func frameJPEG(b []byte) []byte { return b[frameHeader:] }

func serveVideoWS(h *hub, w http.ResponseWriter, r *http.Request) {
	ws, err := wsUpgrade(w, r)
	if err != nil {
		return
	}
	defer ws.Close()
	h.clients.Add(1)
	defer h.clients.Add(-1)

	acks := make(chan struct{}, videoInflight+8)
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			op, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if op != wsOpText {
				continue
			}
			var m struct {
				T string  `json:"t"`
				C float64 `json:"c"`
			}
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			switch m.T {
			case "ack":
				select {
				case acks <- struct{}{}:
				default:
				}
			case "ping":
				resp, _ := json.Marshal(map[string]float64{"c": m.C, "s": serverMs()})
				// {"t":"pong",...}: prepend the type without another map type.
				resp = append([]byte(`{"t":"pong",`), resp[1:]...)
				if ws.WriteMessage(wsOpText, resp) != nil {
					return
				}
			}
		}
	}()

	inflight := 0
	var sent uint64
	for {
		// Wait for a free slot.
		for inflight >= videoInflight {
			select {
			case <-acks:
				inflight = max(0, inflight-1)
			case <-gone:
				return
			}
		}
		b, seq, ch := h.latest()
		if b == nil || seq == sent {
			select {
			case <-ch:
			case <-acks:
				inflight = max(0, inflight-1)
			case <-gone:
				return
			}
			continue
		}
		if err := ws.WriteMessage(wsOpBinary, b); err != nil {
			return
		}
		sent = seq
		inflight++
	}
}
