package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	netWindow  = 1500 * time.Millisecond
	diskWindow = time.Second
	diskLimit  = 1 << 30
	mb         = 1 << 20

	// Throughput in MB/s. Gigabit carries about 117 MB/s of file data.
	fastEthernet = 15.0
	gigabit      = 100.0
	headroom     = 1.1 // a disk this close to the network can become the limit
	wiredRTT     = 5 * time.Millisecond
)

// SpeedReport holds the rates in MB/s of every stage files pass through.
type SpeedReport struct {
	RTT         time.Duration
	NetDown     float64
	NetUp       float64
	ServerRead  float64
	ServerWrite float64
	LocalRead   float64
	LocalWrite  float64
	LocalDir    string
	WinRead     float64 // /mnt/c, where uploads from Windows folders come from
	WinWrite    float64
}

type Alert struct {
	Blocking bool // a real limit on speed, rather than something to know about
	Text     string
}

type stage struct {
	name string
	rate float64
}

func slowest(stages ...stage) stage {
	low := stages[0]
	for _, s := range stages[1:] {
		if s.rate < low.rate {
			low = s
		}
	}
	return low
}

// Evaluate turns measurements into alerts. Each direction is only as fast as its
// slowest stage, so a disk slower than the network is the real limit.
func Evaluate(r SpeedReport) []Alert {
	var alerts []Alert
	link := min(r.NetDown, r.NetUp)
	switch {
	case link < fastEthernet:
		alerts = append(alerts, Alert{true, fmt.Sprintf("Network is at %.0f MB/s, which looks like a 100 Mbps link. Check the cable and the adapter's speed setting.", link)})
	case link < gigabit:
		alerts = append(alerts, Alert{true, fmt.Sprintf("Network is at %.0f MB/s, below gigabit (about 117 MB/s). Check for Wi-Fi, a bad cable or a busy link.", link)})
	}
	if r.RTT > wiredRTT {
		alerts = append(alerts, Alert{false, fmt.Sprintf("Latency is %s. Wired LAN is usually under 1 ms, so this may be Wi-Fi.", r.RTT.Round(100*time.Microsecond))})
	}

	direction := func(label string, net float64, stages ...stage) {
		low := slowest(stages...)
		if low.rate < net {
			alerts = append(alerts, Alert{true, fmt.Sprintf("%s are limited by %s at %.0f MB/s. The network could do %.0f MB/s.", label, low.name, low.rate, net)})
		} else if low.rate < net*headroom {
			alerts = append(alerts, Alert{false, fmt.Sprintf("%s: %s at %.0f MB/s is only just faster than the network.", label, low.name, low.rate)})
		}
	}
	direction("Downloads", r.NetDown, stage{"the server disk", r.ServerRead}, stage{"the local disk", r.LocalWrite})
	direction("Uploads", r.NetUp, stage{"the server disk", r.ServerWrite}, stage{"the local disk", r.LocalRead})
	if r.WinRead > 0 {
		direction("Uploads from /mnt/c", r.NetUp, stage{"/mnt/c", r.WinRead})
	}
	if strings.HasPrefix(r.LocalDir, "/mnt/") {
		alerts = append(alerts, Alert{false, "The download folder is on /mnt, which goes through WSL's slower bridge. Its read speed here may be flattered by the Windows cache."})
	}
	return alerts
}

func (e *Engine) ping() (time.Duration, error) {
	best := time.Duration(0)
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.Server+"/api/speed/ping", nil)
		if err != nil {
			cancel()
			return 0, err
		}
		start := time.Now()
		resp, err := e.client.Do(req)
		if err != nil {
			cancel()
			return 0, err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		cancel()
		if resp.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("server said %s, is it an older version?", resp.Status)
		}
		if d := time.Since(start); best == 0 || d < best {
			best = d
		}
	}
	return best, nil
}

func (e *Engine) netDown() (float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), netWindow+5*time.Second)
	defer cancel()
	url := fmt.Sprintf("%s/api/speed/down?ms=%d", e.Server, netWindow.Milliseconds())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	n, err := io.CopyBuffer(struct{ io.Writer }{io.Discard}, struct{ io.Reader }{resp.Body}, make([]byte, copyBuffer))
	if err != nil {
		return 0, err
	}
	return float64(n) / mb / time.Since(start).Seconds(), nil
}

// timedReader sends a repeating buffer until its deadline, without touching a disk.
type timedReader struct {
	buf   []byte
	until time.Time
	n     int64
}

func (t *timedReader) Read(p []byte) (int, error) {
	if time.Now().After(t.until) {
		return 0, io.EOF
	}
	n := copy(p, t.buf)
	t.n += int64(n)
	return n, nil
}

func (e *Engine) netUp() (float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), netWindow+5*time.Second)
	defer cancel()
	body := &timedReader{buf: make([]byte, copyBuffer), until: time.Now().Add(netWindow)}
	rand.Read(body.buf)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.Server+"/api/speed/up", body)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return float64(body.n) / mb / time.Since(start).Seconds(), nil
}

func (e *Engine) serverDisk() (read, write float64, err error) {
	var out struct {
		Read  float64 `json:"read"`
		Write float64 `json:"write"`
		Error string  `json:"error"`
	}
	if err := e.getJSON("/api/speed/disk", nil, &out); err != nil {
		return 0, 0, err
	}
	if out.Error != "" {
		return 0, 0, errors.New(out.Error)
	}
	return out.Read, out.Write, nil
}

// diskSpeed times a real write, flushed to disk, then a read with the page cache dropped.
func diskSpeed(dir string) (read, write float64, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, 0, err
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	if int64(st.Bavail)*st.Bsize < 2*diskLimit {
		return 0, 0, errors.New("less than 2 GB free")
	}
	f, err := os.CreateTemp(dir, ".gs-speed-*")
	if err != nil {
		return 0, 0, err
	}
	defer os.Remove(f.Name())
	defer f.Close()

	buf := make([]byte, copyBuffer)
	rand.Read(buf)
	start := time.Now()
	var written int64
	for written < diskLimit && time.Since(start) < diskWindow {
		n, err := f.Write(buf)
		if err != nil {
			return 0, 0, err
		}
		written += int64(n)
	}
	if err := f.Sync(); err != nil {
		return 0, 0, err
	}
	write = float64(written) / mb / time.Since(start).Seconds()

	const dontNeed = 4
	syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), 0, 0, dontNeed, 0, 0)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, 0, err
	}
	start = time.Now()
	var total int64
	for time.Since(start) < diskWindow {
		n, err := f.Read(buf)
		total += int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, 0, err
		}
	}
	return float64(total) / mb / time.Since(start).Seconds(), write, nil
}

// SpeedCheck measures every stage and prints each result as it arrives. It returns
// false if the server couldn't be reached, which must never stop a resume.
func (e *Engine) SpeedCheck() (SpeedReport, bool) {
	fmt.Println("Speed check. This tool exists to move files fast, so each stage is timed first.")
	r := SpeedReport{LocalDir: e.Dest}
	row := func(name string, rate float64, err error) {
		if err != nil {
			fmt.Printf("  %-22s failed: %v\n", name, err)
			return
		}
		fmt.Printf("  %-22s %7.0f MB/s\n", name, rate)
	}

	var err error
	if r.RTT, err = e.ping(); err != nil {
		fmt.Printf("  Server unreachable, skipping the check: %v\n", err)
		return r, false
	}
	fmt.Printf("  %-22s %7s\n", "Latency", r.RTT.Round(100*time.Microsecond))

	var failed []string
	check := func(name string, err error) {
		if err != nil {
			failed = append(failed, name)
		}
	}
	r.NetDown, err = e.netDown()
	row("Network down", r.NetDown, err)
	check("network", err)
	r.NetUp, err = e.netUp()
	row("Network up", r.NetUp, err)
	check("network", err)

	r.ServerRead, r.ServerWrite, err = e.serverDisk()
	row("Server disk read", r.ServerRead, err)
	row("Server disk write", r.ServerWrite, err)
	check("server disk", err)

	dir, _ := filepath.Abs(e.Dest)
	r.LocalDir = dir
	r.LocalRead, r.LocalWrite, err = diskSpeed(dir)
	row("Local disk read", r.LocalRead, err)
	row("Local disk write", r.LocalWrite, err)
	check("local disk", err)

	if _, err := os.Stat("/mnt/c/Windows"); err == nil && !strings.HasPrefix(dir, "/mnt/c") {
		r.WinRead, r.WinWrite, err = diskSpeed("/mnt/c/Users/Public")
		row("/mnt/c read", r.WinRead, err)
		row("/mnt/c write", r.WinWrite, err)
	}
	if len(failed) > 0 {
		fmt.Println("  Skipping the verdict: not every stage could be measured.")
		return r, false
	}
	return r, true
}

// Verdict prints the alerts and reports whether any stage really limits speed.
func Verdict(r SpeedReport) bool {
	down := slowest(stage{"network", r.NetDown}, stage{"server disk", r.ServerRead}, stage{"local disk", r.LocalWrite})
	up := slowest(stage{"network", r.NetUp}, stage{"local disk", r.LocalRead}, stage{"server disk", r.ServerWrite})
	fmt.Printf("\n  Downloads: up to %.0f MB/s, set by %s\n  Uploads:   up to %.0f MB/s, set by %s\n\n", down.rate, down.name, up.rate, up.name)
	blocking := false
	for _, a := range Evaluate(r) {
		mark := "note "
		if a.Blocking {
			mark, blocking = "ALERT", true
		}
		fmt.Printf("  %s  %s\n", mark, a.Text)
	}
	return blocking
}
