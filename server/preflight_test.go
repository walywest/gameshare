package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEvaluateStart(t *testing.T) {
	eth := nic{Name: "enp3s0", Mbps: 1000}
	wifi := nic{Name: "wlp2s0", Wireless: true}
	const ok, plenty = 500.0, int64(500 << 30)
	cases := []struct {
		name        string
		nics        []nic
		read, write float64
		free        int64
		block, note string
	}{
		{"healthy", []nic{eth}, ok, ok, plenty, "", ""},
		{"nothing up", nil, ok, ok, plenty, "No physical", ""},
		{"wifi only", []nic{wifi}, ok, ok, plenty, "Only Wi-Fi", ""},
		{"100 Mbps", []nic{{Name: "e", Mbps: 100}}, ok, ok, plenty, "100 Mbps", ""},
		{"wifi beside ethernet", []nic{eth, wifi}, ok, ok, plenty, "", "also up"},
		{"unknown speed assumes gigabit", []nic{{Name: "e"}}, ok, ok, plenty, "", ""},
		{"slow read", []nic{eth}, 80, ok, plenty, "Disk read is 80", ""},
		{"slow write", []nic{eth}, ok, 80, plenty, "Disk write is 80", ""},
		{"tight write", []nic{eth}, ok, 120, plenty, "", "only just faster"},
		{"10 GbE makes a fast disk the limit", []nic{{Name: "e", Mbps: 10000}}, ok, ok, plenty, "slower than the 1175", ""},
		{"disk test skipped", []nic{eth}, 0, 0, plenty, "", ""},
		{"low space", []nic{eth}, ok, ok, 10 << 30, "", "10 GB free"},
	}
	for _, c := range cases {
		var block, notes []string
		for _, a := range evaluateStart(c.nics, c.read, c.write, c.free) {
			if a.Blocking {
				block = append(block, a.Text)
			} else {
				notes = append(notes, a.Text)
			}
		}
		if (c.block == "") != (len(block) == 0) || !strings.Contains(strings.Join(block, "|"), c.block) {
			t.Errorf("%s: alerts %q, want %q", c.name, block, c.block)
		}
		if (c.note == "") != (len(notes) == 0) || !strings.Contains(strings.Join(notes, "|"), c.note) {
			t.Errorf("%s: notes %q, want %q", c.name, notes, c.note)
		}
	}
}

func TestFindNICs(t *testing.T) {
	sys := t.TempDir()
	add := func(name, state, speed string, device, wireless bool) {
		dir := filepath.Join(sys, name)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "operstate"), []byte(state+"\n"), 0o644)
		if speed != "" {
			os.WriteFile(filepath.Join(dir, "speed"), []byte(speed+"\n"), 0o644)
		}
		if device {
			os.MkdirAll(filepath.Join(dir, "device"), 0o755)
		}
		if wireless {
			os.MkdirAll(filepath.Join(dir, "wireless"), 0o755)
		}
	}
	add("enp3s0", "up", "1000", true, false)
	add("wlp2s0", "up", "-1", true, true)
	add("enp4s0", "down", "1000", true, false)
	add("docker0", "up", "10000", false, false)
	add("lo", "unknown", "", false, false)
	want := []nic{{Name: "enp3s0", Mbps: 1000}, {Name: "wlp2s0", Wireless: true}}
	if got := findNICs(sys); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}
