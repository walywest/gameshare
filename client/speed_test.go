package main

import (
	"strings"
	"testing"
	"time"
)

func healthy() SpeedReport {
	return SpeedReport{RTT: time.Millisecond, NetDown: 113, NetUp: 113, ServerRead: 500, ServerWrite: 500, LocalRead: 500, LocalWrite: 500, LocalDir: "/home/a"}
}

func alerts(r SpeedReport) (blocking, notes []string) {
	for _, a := range Evaluate(r) {
		if a.Blocking {
			blocking = append(blocking, a.Text)
		} else {
			notes = append(notes, a.Text)
		}
	}
	return
}

func TestEvaluate(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*SpeedReport)
		block string // expected in a blocking alert, "" for none
		note  string // expected in a note, "" for none
	}{
		{"healthy gigabit", func(*SpeedReport) {}, "", ""},
		{"100 Mbps link", func(r *SpeedReport) { r.NetDown, r.NetUp = 11, 11 }, "100 Mbps", ""},
		{"slow wifi", func(r *SpeedReport) { r.NetDown, r.NetUp = 60, 60 }, "below gigabit", ""},
		{"local write limits downloads", func(r *SpeedReport) { r.LocalWrite = 70 }, "Downloads are limited by the local disk", ""},
		{"server read limits downloads", func(r *SpeedReport) { r.ServerRead = 70 }, "Downloads are limited by the server disk", ""},
		{"server write limits uploads", func(r *SpeedReport) { r.ServerWrite = 70 }, "Uploads are limited by the server disk", ""},
		{"local read limits uploads", func(r *SpeedReport) { r.LocalRead = 70 }, "Uploads are limited by the local disk", ""},
		{"/mnt/c limits uploads", func(r *SpeedReport) { r.WinRead = 70 }, "Uploads from /mnt/c are limited", ""},
		{"/mnt/c tight", func(r *SpeedReport) { r.WinRead = 121 }, "", "only just faster"},
		{"high latency", func(r *SpeedReport) { r.RTT = 20 * time.Millisecond }, "", "Wi-Fi"},
		{"download folder on /mnt", func(r *SpeedReport) { r.LocalDir = "/mnt/d/g" }, "", "bridge"},
	}
	for _, c := range cases {
		r := healthy()
		c.edit(&r)
		block, notes := alerts(r)
		if c.block == "" && len(block) > 0 {
			t.Errorf("%s: unexpected alert %q", c.name, block)
		}
		if c.block != "" && !strings.Contains(strings.Join(block, "|"), c.block) {
			t.Errorf("%s: want alert %q, got %q", c.name, c.block, block)
		}
		if c.note == "" && len(notes) > 0 {
			t.Errorf("%s: unexpected note %q", c.name, notes)
		}
		if c.note != "" && !strings.Contains(strings.Join(notes, "|"), c.note) {
			t.Errorf("%s: want note %q, got %q", c.name, c.note, notes)
		}
	}
}
