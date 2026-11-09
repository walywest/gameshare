package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestParsePaths(t *testing.T) {
	got := parsePaths(`"/mnt/c/My Games/a b" /mnt/d/x 'C:\Program Files\y' ` + "\n")
	want := []string{"/mnt/c/My Games/a b", "/mnt/d/x", `C:\Program Files\y`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestFromWindows(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Program Files\y`: "/mnt/c/Program Files/y",
		`D:\`:                "/mnt/d/",
		"/mnt/c/x":           "/mnt/c/x",
	} {
		if got := fromWindows(in); got != want {
			t.Errorf("%s: got %s", in, got)
		}
	}
}

func TestScroll(t *testing.T) {
	for _, c := range []struct{ cursor, offset, n, rows, want int }{
		{0, 0, 100, 10, 0},
		{9, 0, 100, 10, 0},
		{10, 0, 100, 10, 1},
		{50, 45, 100, 10, 45}, // moving inside the view doesn't scroll
		{44, 45, 100, 10, 44},
		{3, 50, 5, 10, 0}, // list shrank
	} {
		if got := scroll(c.cursor, c.offset, c.n, c.rows); got != c.want {
			t.Errorf("%+v: got %d", c, got)
		}
	}
}

func TestTransferMarks(t *testing.T) {
	views := []View{
		{Item: &Item{Remote: "g/a/x.bin"}, State: active},
		{Item: &Item{Remote: "g/b.bin", Up: true}, State: queued},
		{Item: &Item{Remote: "h/done.bin"}, State: done},
	}
	m := transferMarks(views)
	want := map[string]string{"g/a/x.bin": "↓", "g/a": "↓", "g/b.bin": "↑", "g": "↕"}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("got %v", m)
	}
}

func testModel(w, h int) model {
	m := newModel(NewEngine("http://10.0.0.2:8080", "/home/me/Downloads/GameShare", "/dev/null"))
	m.w, m.h, m.loaded, m.cwd = w, h, true, "games"
	for i := 0; i < 60; i++ {
		m.entries = append(m.entries, Entry{Name: strings.Repeat("Very Long Game Name ", i%4) + fmt.Sprint(i), Dir: i%3 == 0, Size: int64(i) << 30})
	}
	m.applyFilter()
	for i, st := range []string{active, queued, paused, failed, done} {
		it := &Item{ID: i + 1, Remote: "games/Some Game/" + strings.Repeat("deep/", i) + "data.pak", Size: 4 << 30, Up: i%2 == 1}
		m.views = append(m.views, View{Item: it, Size: it.Size, Sent: 1 << 30, Speed: 100 << 20, State: st, Err: map[bool]string{true: "disk full"}[st == failed]})
	}
	m.clamp()
	return m
}

func TestViewFitsTerminal(t *testing.T) {
	for _, sz := range [][2]int{{120, 40}, {80, 24}, {60, 20}, {200, 50}, {40, 12}, {130, 18}} {
		for _, setup := range []func(*model){
			func(*model) {},
			func(m *model) { m.pane = queuePane },
			func(m *model) { m.help = true },
			func(m *model) { m.prompt, m.target = promptDelete, "games/0" },
			func(m *model) { m.cursor[filesPane] = 59; m.clamp() },
		} {
			m := testModel(sz[0], sz[1])
			setup(&m)
			lines := strings.Split(m.View(), "\n")
			if len(lines) > sz[1] {
				t.Errorf("%v: %d lines", sz, len(lines))
			}
			for i, l := range lines {
				if w := lipgloss.Width(l); w > sz[0] {
					t.Errorf("%v line %d is %d wide: %q", sz, i, w, l)
				}
			}
		}
	}
}

func press(m model, keys ...string) model {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "left":
			msg = tea.KeyMsg{Type: tea.KeyLeft}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		next, _ := m.Update(msg)
		m = next.(model)
	}
	return m
}

func TestDeleteNeedsConfirmation(t *testing.T) {
	m := press(testModel(100, 30), "j", "x")
	if m.prompt != promptDelete || m.target != "games/Very Long Game Name 1" {
		t.Fatalf("prompt %v target %q", m.prompt, m.target)
	}
	m = press(m, "n")
	if m.prompt != noPrompt || m.target != "" || m.note != "Delete cancelled" {
		t.Fatalf("not cancelled: %v %q %q", m.prompt, m.target, m.note)
	}
}

func TestFilter(t *testing.T) {
	m := press(testModel(100, 30), "/", "5", "9")
	if len(m.shown) != 1 || m.shown[0].Name != "Very Long Game Name Very Long Game Name Very Long Game Name 59" {
		t.Fatalf("shown %v", m.shown)
	}
	m = press(m, "enter")
	if m.prompt != noPrompt || len(m.shown) != 1 {
		t.Fatal("enter should keep the filter")
	}
	m = press(m, "esc")
	if len(m.shown) != 60 {
		t.Fatal("esc should clear the filter")
	}
}

func TestBackSelectsFolderWeLeft(t *testing.T) {
	m := testModel(100, 30)
	m.cwd, m.want = "games/sub", "games/sub"
	m = press(m, "left")
	next, _ := m.Update(listMsg{dir: "games", entries: []Entry{{Name: "a", Dir: true}, {Name: "sub", Dir: true}, {Name: "z"}}})
	m = next.(model)
	if m.cwd != "games" || m.cursor[filesPane] != 1 {
		t.Fatalf("cwd %q cursor %d", m.cwd, m.cursor[filesPane])
	}
	// A late reply for a folder we already left is ignored.
	next, _ = m.Update(listMsg{dir: "games/sub"})
	if next.(model).cwd != "games" {
		t.Fatal("stale listing applied")
	}
}

func TestUpdateSummary(t *testing.T) {
	cases := []struct {
		r    UpdateResult
		want string
		err  bool
	}{
		{UpdateResult{From: "a", To: "a"}, "already up to date", false},
		{UpdateResult{From: "a", To: "b", Log: "b one"}, "1 new commit)", false},
		{UpdateResult{From: "a", To: "b", Log: "b one\nc two", ServerCode: true, Restarting: true}, "Server restarting", false},
		{UpdateResult{From: "a", To: "b", Log: "b", ServerCode: true, BuildErr: "x.go:1: bad\nmore"}, "old server keeps running: x.go:1: bad", true},
	}
	for _, c := range cases {
		got, isErr := c.r.Summary()
		if !strings.Contains(got, c.want) || isErr != c.err || strings.Contains(got, "more") {
			t.Errorf("%+v: %q %v", c.r, got, isErr)
		}
	}
}
