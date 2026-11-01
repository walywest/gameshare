package main

import (
	"fmt"
	"path"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type tickMsg struct{}

type listMsg struct {
	dir     string
	entries []Entry
	free    int64
	err     error
}

type noteMsg string

type model struct {
	e       *Engine
	cwd     string
	entries []Entry
	free    int64
	views   []View
	inQueue bool
	cursor  [2]int // browser, queue
	note    string
	input   *string // non-nil while typing a path to upload
	w, h    int
}

func newModel(e *Engine) model { return model{e: e, w: 100, h: 30} }

func (m model) load() tea.Cmd {
	dir, e := m.cwd, m.e
	return func() tea.Msg {
		entries, free, err := e.List(dir)
		return listMsg{dir, entries, free, err}
	}
}

func tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m model) Init() tea.Cmd { return tea.Batch(m.load(), tick()) }

func (m *model) upload(text string) tea.Cmd {
	e, dir := m.e, m.cwd
	paths := parsePaths(text)
	for i, p := range paths {
		paths[i] = fromWindows(p)
	}
	return func() tea.Msg {
		total := 0
		for _, p := range paths {
			n, err := e.QueueUpload(p, dir)
			if err != nil {
				return noteMsg(err.Error())
			}
			total += n
		}
		return noteMsg(fmt.Sprintf("Queued %d files to upload", total))
	}
}

func (m model) download(remote string) tea.Cmd {
	e, base := m.e, m.cwd
	return func() tea.Msg {
		n, err := e.QueueDownload(base, remote)
		if err != nil {
			return noteMsg(err.Error())
		}
		return noteMsg(fmt.Sprintf("Queued %d files to download", n))
	}
}

// parsePaths splits what a terminal pastes when files are dropped on it: paths
// separated by spaces, with quotes around any that contain spaces.
func parsePaths(text string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	var quote rune
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range text {
		switch {
		case quoted && r == quote:
			quoted = false
		case !quoted && (r == '"' || r == '\''):
			quoted, quote = true, r
		case !quoted && (r == ' ' || r == '\n' || r == '\r' || r == '\t'):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// fromWindows turns a pasted C:\\dir\\file into /mnt/c/dir/file, since the client runs in WSL.
func fromWindows(p string) string {
	if len(p) < 3 || p[1] != ':' || p[2] != '\\' {
		return p
	}
	return "/mnt/" + strings.ToLower(p[:1]) + strings.ReplaceAll(p[2:], "\\", "/")
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tickMsg:
		before := countDone(m.views)
		m.views = m.e.Snapshot()
		m.clamp()
		if countDone(m.views) != before { // an upload may have added a file
			return m, tea.Batch(tick(), m.load())
		}
		return m, tick()
	case listMsg:
		if msg.err != nil {
			m.note = "Server unreachable: " + msg.err.Error()
			return m, nil
		}
		m.cwd, m.entries, m.free = msg.dir, msg.entries, msg.free
		m.clamp()
	case noteMsg:
		m.note = string(msg)
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func countDone(views []View) int {
	n := 0
	for _, v := range views {
		if v.State == done {
			n++
		}
	}
	return n
}

func (m *model) clamp() {
	m.cursor[0] = min(m.cursor[0], max(len(m.entries)-1, 0))
	m.cursor[1] = min(m.cursor[1], max(len(m.views)-1, 0))
}

func (m model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.Paste && m.input == nil { // files dropped on the terminal
		return m, m.upload(string(k.Runes))
	}
	if m.input != nil {
		switch k.Type {
		case tea.KeyEnter:
			text := *m.input
			m.input = nil
			return m, m.upload(text)
		case tea.KeyEsc:
			m.input = nil
		case tea.KeyBackspace:
			if r := []rune(*m.input); len(r) > 0 {
				s := string(r[:len(r)-1])
				m.input = &s
			}
		case tea.KeyRunes, tea.KeySpace:
			s := *m.input + string(k.Runes)
			m.input = &s
		}
		return m, nil
	}

	pane := 0
	size := len(m.entries)
	if m.inQueue {
		pane, size = 1, len(m.views)
	}
	switch k.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab":
		m.inQueue = !m.inQueue
	case "up", "k":
		m.cursor[pane] = max(m.cursor[pane]-1, 0)
	case "down", "j":
		m.cursor[pane] = min(m.cursor[pane]+1, max(size-1, 0))
	case "u":
		s := ""
		m.input = &s
	case "c":
		m.e.ClearDone()
	default:
		if m.inQueue {
			return m.queueKey(k)
		}
		return m.browserKey(k)
	}
	return m, nil
}

func (m model) browserKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "enter":
		if len(m.entries) > 0 && m.entries[m.cursor[0]].Dir {
			m.cwd = path.Join(m.cwd, m.entries[m.cursor[0]].Name)
			m.cursor[0] = 0
			return m, m.load()
		}
	case "backspace", "left":
		if m.cwd != "" {
			m.cwd = strings.TrimPrefix(path.Dir("/"+m.cwd), "/")
			m.cursor[0] = 0
			return m, m.load()
		}
	case "d":
		if len(m.entries) > 0 {
			return m, m.download(path.Join(m.cwd, m.entries[m.cursor[0]].Name))
		}
	case "a":
		return m, m.download(m.cwd)
	case "r":
		return m, m.load()
	}
	return m, nil
}

func (m model) queueKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if len(m.views) == 0 {
		return m, nil
	}
	id := m.views[m.cursor[1]].Item.ID
	switch k.String() {
	case " ", "p":
		m.e.Toggle(id)
	case "n":
		m.e.Next(id)
	case "x":
		m.e.Remove(id)
	}
	return m, nil
}

func bytes_(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v, i := float64(n), 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i > 0 && v < 100 {
		return fmt.Sprintf("%.1f %s", v, units[i])
	}
	return fmt.Sprintf("%.0f %s", v, units[i])
}

func eta(left int64, speed float64) string {
	if speed < 1 {
		return ""
	}
	return (time.Duration(float64(left)/speed) * time.Second).Round(time.Second).String()
}

func fit(s string, w int) string {
	r := []rune(s)
	if w < 1 {
		return ""
	}
	if len(r) > w {
		return "…" + string(r[len(r)-w+1:])
	}
	return s + strings.Repeat(" ", w-len(r))
}

func bar(frac float64, w int) string {
	full := int(frac * float64(w))
	return strings.Repeat("█", min(full, w)) + strings.Repeat("░", w-min(full, w))
}

// window returns the slice of n rows that keeps the cursor in view.
func window(cursor, n, rows int) (int, int) {
	start := max(cursor-rows+1, 0)
	return start, min(start+rows, n)
}

func (m model) rowsBrowser(w, rows int) []string {
	lines := []string{}
	start, end := window(m.cursor[0], len(m.entries), rows)
	for i := start; i < end; i++ {
		e := m.entries[i]
		size := ""
		if !e.Dir {
			size = bytes_(e.Size)
		}
		name := e.Name
		if e.Dir {
			name += "/"
		}
		line := fit(name, w-11) + fmt.Sprintf("%10s ", size)
		if i == m.cursor[0] && !m.inQueue {
			line = "\x1b[7m" + line + "\x1b[0m"
		}
		lines = append(lines, line)
	}
	if len(m.entries) == 0 {
		lines = append(lines, "  (empty)")
	}
	return lines
}

func (m model) rowsQueue(w, rows int) []string {
	lines := []string{}
	start, end := window(m.cursor[1], len(m.views), rows/2)
	for i := start; i < end; i++ {
		v := m.views[i]
		arrow := "↓"
		if v.Item.Up {
			arrow = "↑"
		}
		name := fit(arrow+" "+v.Item.Remote, w)
		if i == m.cursor[1] && m.inQueue {
			name = "\x1b[7m" + name + "\x1b[0m"
		}
		frac := 0.0
		if v.Size > 0 {
			frac = float64(v.Sent) / float64(v.Size)
		}
		info := fmt.Sprintf("%s of %s", bytes_(v.Sent), bytes_(v.Size))
		switch {
		case v.Err != "":
			info = v.Err
		case v.State == active && v.Speed > 0:
			info += fmt.Sprintf(" %s/s %s", bytes_(int64(v.Speed)), eta(v.Size-v.Sent, v.Speed))
		case v.State != active:
			info = v.State + " " + info
		}
		lines = append(lines, name, fit(bar(frac, 12)+" "+info, w))
	}
	if len(m.views) == 0 {
		lines = append(lines, "  (nothing queued)")
	}
	return lines
}

func (m model) View() string {
	left := max(m.w/2-1, 20)
	right := max(m.w-left-3, 20)
	rows := max(m.h-5, 4)
	a, b := m.rowsBrowser(left, rows), m.rowsQueue(right, rows)

	var out strings.Builder
	var rate float64
	for _, v := range m.views {
		rate += v.Speed
	}
	status := fmt.Sprintf("%s free  %s/s  %s", bytes_(m.free), bytes_(int64(rate)), m.e.Dest)
	fmt.Fprintf(&out, "%s   %s\n", fit("/"+m.cwd, left), fit(status, right))
	out.WriteString(strings.Repeat("─", m.w) + "\n")
	for i := 0; i < rows; i++ {
		l, r := strings.Repeat(" ", left), ""
		if i < len(a) {
			l = a[i]
		}
		if i < len(b) {
			r = b[i]
		}
		out.WriteString(l + " │ " + r + "\n")
	}
	out.WriteString(strings.Repeat("─", m.w) + "\n")
	switch {
	case m.input != nil:
		out.WriteString("Upload path (or drop files here): " + *m.input + "▌")
	case m.note != "":
		out.WriteString(m.note + "\n")
		fallthrough
	default:
		if m.inQueue {
			out.WriteString("tab files  space pause/resume  n next  x remove  c clear done  q quit")
		} else {
			out.WriteString("tab queue  enter open  ← back  d download  a all here  u upload  r refresh  q quit")
		}
	}
	return out.String()
}
