package main

import (
	"log"
	"time"
)

// runSpin busy-loops and reports iterations per second. Run it at low
// priority (nice 19) beside a capture to measure how much CPU is left idle,
// since /proc/stat accounting is unreliable on this board.
func runSpin(dur time.Duration) {
	end := time.Now().Add(dur)
	for time.Now().Before(end) {
		t0 := time.Now()
		n := 0
		for time.Since(t0) < time.Second {
			for i := 0; i < 10000; i++ {
				n++
			}
		}
		log.Printf("spin %d", n/10000)
	}
}
