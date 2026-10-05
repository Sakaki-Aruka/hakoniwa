package main

// Session management: "start" launches "hakoniwa serve" in the background,
// which registers itself as <runtime dir>/hakoniwa/<port>.json once it is
// listening and removes the file when it exits. "list" and "close" work on
// those files.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

const defaultPort = 8080

type session struct {
	PID     int       `json:"pid"`
	Port    int       `json:"port"`
	Listen  string    `json:"listen"`
	HID     string    `json:"hid"`
	Video   string    `json:"video"`
	Started time.Time `json:"started"`
}

func sessionDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "hakoniwa")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("hakoniwa-%d", os.Getuid()))
}

func sessionFile(port int) string { return filepath.Join(sessionDir(), fmt.Sprintf("%d.json", port)) }
func logFile(port int) string     { return filepath.Join(sessionDir(), fmt.Sprintf("%d.log", port)) }

func (s *session) save() error {
	if err := os.MkdirAll(sessionDir(), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	tmp := sessionFile(s.Port) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, sessionFile(s.Port))
}

// remove deletes the session file if it still belongs to this session.
func (s *session) remove() {
	if cur, err := readSession(sessionFile(s.Port)); err == nil && cur.PID == s.PID {
		os.Remove(sessionFile(s.Port))
	}
}

func readSession(path string) (*session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// alive reports whether the session's process is still a running hakoniwa.
func (s *session) alive() bool {
	if err := syscall.Kill(s.PID, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", s.PID))
	return err != nil || strings.Contains(string(cmdline), "serve") // PID reused by something else?
}

// loadSessions returns the running sessions sorted by port, removing files
// left behind by sessions that died.
func loadSessions() []*session {
	files, _ := filepath.Glob(filepath.Join(sessionDir(), "*.json"))
	var out []*session
	for _, f := range files {
		s, err := readSession(f)
		if err != nil {
			continue
		}
		if !s.alive() {
			os.Remove(f)
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

func (s *session) url() string {
	host, port, _ := net.SplitHostPort(s.Listen)
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		if h, err := os.Hostname(); err == nil {
			host = h
		} else {
			host = "localhost"
		}
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

// parseInterspersed parses flags that may appear before, between or after
// positional arguments.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func cmdStart(args []string) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usageText) }
	bind := fs.String("bind", "127.0.0.1", "address to listen on")
	size := fs.String("size", "1920x1080", "initial resolution")
	audio := fs.String("audio", "auto", `ALSA capture device, "auto" or "none"`)
	backend := fs.String("backend", "v4l2", "capture backend: v4l2 or usbfs")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) < 2 || len(pos) > 3 {
		fmt.Fprintln(os.Stderr, "usage: hakoniwa start [options] <esp32-device> <video-device> [port]")
		return 2
	}
	hid, err := resolveDevice(pos[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "hakoniwa: ESP32 device: %v\n", err)
		return 1
	}
	video, err := resolveDevice(pos[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "hakoniwa: video device: %v\n", err)
		return 1
	}
	if !isVideoCapture(video) {
		fmt.Fprintf(os.Stderr, "hakoniwa: %s is not a V4L2 video device\n", video)
		return 1
	}
	port := defaultPort
	if len(pos) == 3 {
		port, err = strconv.Atoi(pos[2])
		if err != nil || port < 1 || port > 65535 {
			fmt.Fprintf(os.Stderr, "hakoniwa: invalid port %q\n", pos[2])
			return 2
		}
	}
	for _, s := range loadSessions() {
		switch {
		case s.Port == port:
			fmt.Fprintf(os.Stderr, "hakoniwa: port %d is already used by a session (pid %d)\n", port, s.PID)
			return 1
		case realDevice(s.HID) == realDevice(hid):
			fmt.Fprintf(os.Stderr, "hakoniwa: %s is already used by the session on port %d\n", hid, s.Port)
			return 1
		case realDevice(s.Video) == realDevice(video):
			fmt.Fprintf(os.Stderr, "hakoniwa: %s is already used by the session on port %d\n", video, s.Port)
			return 1
		}
	}

	audioDev := *audio
	if audioDev == "none" {
		audioDev = ""
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hakoniwa: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(sessionDir(), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "hakoniwa: %v\n", err)
		return 1
	}
	logPath := logFile(port)
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hakoniwa: %v\n", err)
		return 1
	}
	defer lf.Close()

	cmd := exec.Command(exe, "serve",
		"-dev", video, "-hid", hid, "-backend", *backend, "-size", *size,
		"-audiodev", audioDev, "-listen", net.JoinHostPort(*bind, strconv.Itoa(port)))
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survive the terminal
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "hakoniwa: %v\n", err)
		return 1
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// Ready once the session has registered itself (i.e. it is listening).
	deadline := time.After(10 * time.Second)
	for {
		select {
		case err := <-exited:
			fmt.Fprintf(os.Stderr, "hakoniwa: session failed to start (%v)\n%s", err, tail(logPath, 10))
			return 1
		case <-deadline:
			cmd.Process.Kill()
			fmt.Fprintf(os.Stderr, "hakoniwa: session did not start within 10s\n%s", tail(logPath, 10))
			return 1
		case <-time.After(100 * time.Millisecond):
		}
		if s, err := readSession(sessionFile(port)); err == nil && s.PID == cmd.Process.Pid {
			fmt.Printf("started session on %s (pid %d)\n  ESP32: %s\n  video: %s\n  log:   %s\n",
				s.url(), s.PID, hid, video, logPath)
			return 0
		}
	}
}

// tail returns the last n lines of a file, indented, for error messages.
func tail(path string, n int) string {
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	var b strings.Builder
	for _, l := range lines {
		if l != "" {
			b.WriteString("  | " + l + "\n")
		}
	}
	return b.String()
}

func cmdClose(args []string) int {
	if len(args) > 1 {
		fmt.Fprintln(os.Stderr, "usage: hakoniwa close [port]")
		return 2
	}
	sessions := loadSessions()
	if len(args) == 1 {
		port, err := strconv.Atoi(args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hakoniwa: invalid port %q\n", args[0])
			return 2
		}
		var match []*session
		for _, s := range sessions {
			if s.Port == port {
				match = append(match, s)
			}
		}
		if len(match) == 0 {
			fmt.Fprintf(os.Stderr, "hakoniwa: no session on port %d\n", port)
			return 1
		}
		sessions = match
	} else if len(sessions) == 0 {
		fmt.Println("no sessions")
		return 0
	}
	code := 0
	for _, s := range sessions {
		if err := stopSession(s); err != nil {
			fmt.Fprintf(os.Stderr, "hakoniwa: port %d (pid %d): %v\n", s.Port, s.PID, err)
			code = 1
			continue
		}
		fmt.Printf("closed session on port %d (pid %d)\n", s.Port, s.PID)
	}
	return code
}

// stopSession asks the session to exit (SIGTERM), waiting up to 5 s before
// killing it.
func stopSession(s *session) error {
	if err := syscall.Kill(s.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	for i := 0; i < 50; i++ {
		if !s.alive() {
			s.remove()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	syscall.Kill(s.PID, syscall.SIGKILL)
	time.Sleep(200 * time.Millisecond)
	s.remove()
	if s.alive() {
		return fmt.Errorf("process did not exit")
	}
	return nil
}

func cmdList(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: hakoniwa list")
		return 2
	}
	sessions := loadSessions()
	if len(sessions) == 0 {
		fmt.Println("no sessions")
		return 0
	}
	printSessions(os.Stdout, sessions)
	return 0
}

func printSessions(w io.Writer, sessions []*session) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PORT\tPID\tURL\tESP32\tVIDEO\tUPTIME")
	for _, s := range sessions {
		fmt.Fprintf(tw, "%d\t%d\t%s\t%s\t%s\t%s\n", s.Port, s.PID, s.url(), s.HID, s.Video,
			time.Since(s.Started).Round(time.Second))
	}
	tw.Flush()
}
