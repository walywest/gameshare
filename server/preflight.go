package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	gigabitMbps = 1000
	wireEff     = 0.94 // share of a link's rate left for file data after TCP/IP overhead
	headroom    = 1.1  // a disk this close to the link can become the limit
	lowFree     = 50 << 30
)

type nic struct {
	Name     string
	Mbps     int // 0 when the driver doesn't report it
	Wireless bool
}

// findNICs lists physical interfaces that are up. Virtual ones, such as bridges and
// VPNs, have no "device" entry in sysfs.
func findNICs(sys string) []nic {
	entries, err := os.ReadDir(sys)
	if err != nil {
		return nil
	}
	var nics []nic
	for _, e := range entries {
		dir := filepath.Join(sys, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "device")); err != nil {
			continue
		}
		state, err := os.ReadFile(filepath.Join(dir, "operstate"))
		if err != nil || strings.TrimSpace(string(state)) != "up" {
			continue
		}
		n := nic{Name: e.Name()}
		if _, err := os.Stat(filepath.Join(dir, "wireless")); err == nil {
			n.Wireless = true
		}
		if raw, err := os.ReadFile(filepath.Join(dir, "speed")); err == nil {
			if v, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && v > 0 {
				n.Mbps = v
			}
		}
		nics = append(nics, n)
	}
	return nics
}

type alert struct {
	Blocking bool // a real limit on speed, rather than something to know about
	Text     string
}

// evaluateStart judges what the server can see alone. Real network throughput needs
// both ends, so gs measures that when it starts.
func evaluateStart(nics []nic, read, write float64, free int64) []alert {
	var alerts []alert
	best, wired, wifi := 0, 0, ""
	for _, n := range nics {
		if n.Wireless {
			wifi = n.Name
			continue
		}
		wired++
		best = max(best, n.Mbps)
	}
	switch {
	case len(nics) == 0:
		alerts = append(alerts, alert{true, "No physical network interface is up."})
	case wired == 0:
		alerts = append(alerts, alert{true, fmt.Sprintf("Only Wi-Fi is up (%s). Plug in Ethernet for full speed.", wifi)})
	case best > 0 && best < gigabitMbps:
		alerts = append(alerts, alert{true, fmt.Sprintf("Ethernet negotiated %d Mbps, below gigabit. Check the cable and the switch.", best)})
	case wifi != "":
		alerts = append(alerts, alert{false, fmt.Sprintf("Wi-Fi (%s) is also up. Use the Ethernet address so transfers don't go over it.", wifi)})
	}

	if best == 0 {
		best = gigabitMbps
	}
	link := float64(best) * wireEff / 8
	disk := func(label string, rate float64) {
		if rate <= 0 {
			return
		}
		if rate < link {
			alerts = append(alerts, alert{true, fmt.Sprintf("Disk %s is %.0f MB/s, slower than the %.0f MB/s link.", label, rate, link)})
		} else if rate < link*headroom {
			alerts = append(alerts, alert{false, fmt.Sprintf("Disk %s at %.0f MB/s is only just faster than the %.0f MB/s link.", label, rate, link)})
		}
	}
	disk("read", read)
	disk("write", write)
	if free < lowFree {
		alerts = append(alerts, alert{false, fmt.Sprintf("Only %d GB free in the shared folder.", free>>30)})
	}
	return alerts
}

// preflight prints the checks. It never stops the server: nobody may be watching.
func preflight() {
	fmt.Println("Speed check. This tool exists to move files fast, so the server tests itself first.")
	nics := findNICs("/sys/class/net")
	for _, n := range nics {
		kind, speed := "Ethernet", "speed unknown"
		if n.Wireless {
			kind = "Wi-Fi"
		}
		if n.Mbps > 0 {
			speed = fmt.Sprintf("%d Mbps", n.Mbps)
		}
		fmt.Printf("  %-22s %s, %s\n", n.Name, kind, speed)
	}
	read, write, err := diskSpeed()
	if err != nil {
		fmt.Printf("  Disk test skipped: %v\n", err)
	} else {
		fmt.Printf("  %-22s %7.0f MB/s\n  %-22s %7.0f MB/s\n", "Disk read", read, "Disk write", write)
	}
	free := freeBytes()
	fmt.Printf("  %-22s %7d GB\n", "Free space", free>>30)
	for _, a := range evaluateStart(nics, read, write, free) {
		mark := "note "
		if a.Blocking {
			mark = "ALERT"
		}
		fmt.Printf("  %s  %s\n", mark, a.Text)
	}
	fmt.Println("  Run gs to measure the network itself.")
}
