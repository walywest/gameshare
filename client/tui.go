package main

import (
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

type tickMsg struct{}

type listMsg struct {
	dir     string
	entries []Entry
	free    int64
	err     error
}

type noteMsg struct {
	text string
	err  bool
}

type deletedMsg struct {
	remote string
	err    error
}

type promptKind int

const (
	noPrompt promptKind = iota
	promptUpload
	promptFilter
	promptDelete
)

const (
	filesPane = 0
	queuePane = 1
)

type model struct {
	e       *Engine
	cwd     string
	want    string // folder being loaded; replies for any other are stale
	back    string // entry to select once the parent folder loads
	loaded  bool
	entries []Entry
	shown   []Entry // entries matching filter
	filter  string
	free    int64
	views   []View
	marks   map[string]string // remote path -> ↓/↑ while it has unfinished transfers

	pane   int
	cursor [2]int
	offset [2]int

	note    string
	noteErr bool
	noteAt  time.Time

	prompt promptKind
	input  string
	target string // remote path awaiting delete confirmation
	help   bool
	w, h   int
}

func newModel(e *Engine) model { return model{e: e, w: 100, h: 30} }

func (m *model) loadDir(dir string) tea.Cmd {
	m.want = dir
	e := m.e
	return func() tea.Msg {
		entries, free, err := e.List(dir)
		return listMsg{dir, entries, free, err}
	}
}

func tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m model) Init() tea.Cmd { return tea.Batch(m.loadDir(""), tick()) }

func (m *model) setNote(text string, err bool) {
	m.note, m.noteErr, m.noteAt = text, err, time.Now()
}

func (m *model) upload(text string) tea.Cmd {
	e, dir := m.e, m.cwd
	paths := parsePaths(text)
	for i, p := range paths {
		paths[i] = fromWindows(p)
	}
	if len(paths) == 0 {
		return nil
	}
	return func() tea.Msg {
		total := 0
		for _, p := range paths {
			n, err := e.QueueUpload(p, dir)
			if err != nil {
				return noteMsg{err.Error(), true}
			}
			total += n
		}
		return noteMsg{fmt.Sprintf("Queued %s to upload into /%s", plural(total, "file"), dir), false}
	}
}

func (m model) download(remote string) tea.Cmd {
	e, base := m.e, m.cwd
	return func() tea.Msg {
		n, err := e.QueueDownload(base, remote)
		if err != nil {
			return noteMsg{err.Error(), true}
		}
		return noteMsg{fmt.Sprintf("Queued %s to download", plural(n, "file")), false}
	}
}

func (m model) remove(remote string) tea.Cmd {
	e := m.e
	return func() tea.Msg { return deletedMsg{remote, e.Delete(remote)} }
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
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

/* Update */

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.clamp()
	case tickMsg:
		before := countDone(m.views)
		m.views = m.e.Snapshot()
		m.marks = transferMarks(m.views)
		m.clamp()
		if m.note != "" && time.Since(m.noteAt) > noteLife(m.noteErr) {
			m.note = ""
		}
		if countDone(m.views) != before { // an upload may have added a file
			return m, tea.Batch(tick(), m.loadDir(m.cwd))
		}
		return m, tick()
	case listMsg:
		if msg.dir != m.want {
			return m, nil
		}
		if msg.err != nil {
			m.setNote("Can't list /"+msg.dir+": "+msg.err.Error(), true)
			m.want, m.back = m.cwd, ""
			return m, nil
		}
		if msg.dir != m.cwd {
			m.filter = ""
			m.cursor[filesPane], m.offset[filesPane] = 0, 0
		}
		m.cwd, m.entries, m.free, m.loaded = msg.dir, msg.entries, msg.free, true
		m.applyFilter()
		if m.back != "" {
			for i, e := range m.shown {
				if e.Name == m.back {
					m.cursor[filesPane] = i
				}
			}
			m.back = ""
		}
		m.clamp()
	case noteMsg:
		m.setNote(msg.text, msg.err)
	case deletedMsg:
		if msg.err != nil {
			m.setNote("Couldn't delete /"+msg.remote+": "+msg.err.Error(), true)
			return m, nil
		}
		m.setNote("Deleted /"+msg.remote+" from the server", false)
		return m, m.loadDir(m.cwd)
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func noteLife(err bool) time.Duration {
	if err {
		return 12 * time.Second
	}
	return 6 * time.Second
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

// transferMarks flags each unfinished transfer's path and the folders above it, so
// the file list can show what is on its way.
func transferMarks(views []View) map[string]string {
	marks := map[string]string{}
	for _, v := range views {
		if v.State == done {
			continue
		}
		mark := "↓"
		if v.Item.Up {
			mark = "↑"
		}
		for p := v.Item.Remote; p != "." && p != "/" && p != ""; p = path.Dir(p) {
			if _, ok := marks[p]; ok && marks[p] != mark {
				marks[p] = "↕"
			} else if !ok {
				marks[p] = mark
			}
		}
	}
	return marks
}

func (m *model) applyFilter() {
	if m.filter == "" {
		m.shown = m.entries
		return
	}
	f := strings.ToLower(m.filter)
	m.shown = nil
	for _, e := range m.entries {
		if strings.Contains(strings.ToLower(e.Name), f) {
			m.shown = append(m.shown, e)
		}
	}
}

func (m *model) count(pane int) int {
	if pane == queuePane {
		return len(m.views)
	}
	return len(m.shown)
}

// scroll keeps the cursor on screen without moving the view more than it must.
func scroll(cursor, offset, n, rows int) int {
	rows = max(rows, 1)
	if cursor < offset {
		offset = cursor
	}
	if cursor >= offset+rows {
		offset = cursor - rows + 1
	}
	return max(min(offset, n-rows), 0)
}

func (m *model) clamp() {
	l := m.layout()
	for pane, rows := range [2]int{l.fileRows, l.queueSlots} {
		n := m.count(pane)
		m.cursor[pane] = max(min(m.cursor[pane], n-1), 0)
		m.offset[pane] = scroll(m.cursor[pane], m.offset[pane], n, rows)
	}
}

func (m *model) move(delta int) {
	p := m.pane
	m.cursor[p] = max(min(m.cursor[p]+delta, m.count(p)-1), 0)
	m.clamp()
}

func (m *model) selected() (Entry, bool) {
	if len(m.shown) == 0 {
		return Entry{}, false
	}
	return m.shown[m.cursor[filesPane]], true
}

func (m model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.String() == "ctrl+c" {
		return m, tea.Quit
	}
	if m.prompt != noPrompt {
		return m.promptKey(k)
	}
	if k.Paste { // files dropped on the terminal
		return m, m.upload(string(k.Runes))
	}
	if m.help {
		m.help = false
		return m, nil
	}

	l := m.layout()
	page := l.fileRows
	if m.pane == queuePane {
		page = l.queueSlots
	}
	switch k.String() {
	case "q":
		return m, tea.Quit
	case "?":
		m.help = true
	case "tab", "shift+tab":
		m.pane = 1 - m.pane
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "pgup", "ctrl+u":
		m.move(-page)
	case "pgdown", "ctrl+d":
		m.move(page)
	case "home", "g":
		m.move(-m.count(m.pane))
	case "end", "G":
		m.move(m.count(m.pane))
	case "u":
		m.prompt, m.input = promptUpload, ""
	case "c":
		m.e.ClearDone()
		m.views = m.e.Snapshot()
		m.clamp()
	default:
		if m.pane == queuePane {
			return m.queueKey(k)
		}
		return m.browserKey(k)
	}
	return m, nil
}

func (m model) promptKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.prompt == promptDelete {
		target := m.target
		m.prompt, m.target = noPrompt, ""
		if k.String() == "y" || k.String() == "Y" {
			m.setNote("Deleting /"+target+"…", false)
			return m, m.remove(target)
		}
		m.setNote("Delete cancelled", false)
		return m, nil
	}

	switch k.Type {
	case tea.KeyEnter:
		kind := m.prompt
		m.prompt = noPrompt
		if kind == promptFilter {
			return m, nil // already applied while typing
		}
		text := m.input
		m.input = ""
		return m, m.upload(text)
	case tea.KeyEsc:
		if m.prompt == promptFilter {
			m.filter = ""
			m.applyFilter()
			m.clamp()
		}
		m.prompt = noPrompt
		return m, nil
	case tea.KeyBackspace:
		if r := []rune(m.input); len(r) > 0 {
			m.input = string(r[:len(r)-1])
		}
	case tea.KeyCtrlU:
		m.input = ""
	case tea.KeyRunes, tea.KeySpace:
		m.input += string(k.Runes)
	default:
		return m, nil
	}
	if m.prompt == promptFilter {
		m.filter = m.input
		m.applyFilter()
		m.cursor[filesPane] = 0
		m.clamp()
	}
	return m, nil
}

func (m model) browserKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	sel, ok := m.selected()
	switch k.String() {
	case "enter", "right", "l":
		if ok && sel.Dir {
			return m, m.loadDir(path.Join(m.cwd, sel.Name))
		}
		if ok && k.String() == "enter" {
			m.setNote("Press d to download this file", false)
		}
	case "backspace", "left", "h":
		if m.cwd != "" {
			m.back = path.Base(m.cwd)
			return m, m.loadDir(parentOf(m.cwd))
		}
	case "~":
		if m.cwd != "" {
			return m, m.loadDir("")
		}
	case "esc":
		if m.filter != "" {
			m.filter = ""
			m.applyFilter()
			m.clamp()
		}
	case "/":
		m.prompt, m.input = promptFilter, m.filter
	case "d":
		if ok {
			return m, m.download(path.Join(m.cwd, sel.Name))
		}
	case "a":
		return m, m.download(m.cwd)
	case "r":
		return m, m.loadDir(m.cwd)
	case "x", "delete":
		if ok {
			m.prompt, m.target = promptDelete, path.Join(m.cwd, sel.Name)
		}
	}
	return m, nil
}

func (m model) queueKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if len(m.views) == 0 {
		return m, nil
	}
	v := m.views[m.cursor[queuePane]]
	switch k.String() {
	case " ", "p":
		m.e.Toggle(v.Item.ID)
	case "n":
		m.e.Next(v.Item.ID)
		m.setNote("Running "+path.Base(v.Item.Remote)+" next", false)
	case "x", "delete":
		m.e.Remove(v.Item.ID)
	case "o", "enter":
		// Jump the file list to the folder this item lives in.
		m.pane = filesPane
		m.back = path.Base(v.Item.Remote)
		return m, m.loadDir(parentOf(v.Item.Remote))
	}
	m.views = m.e.Snapshot()
	m.clamp()
	return m, nil
}

/* Formatting helpers */

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

// fit pads or cuts s to exactly w columns, cutting from the left so a path keeps
// its file name.
func fit(s string, w int) string {
	if w < 1 {
		return ""
	}
	if runewidth.StringWidth(s) > w {
		s = runewidth.TruncateLeft(s, runewidth.StringWidth(s)-w+1, "…")
	}
	return runewidth.FillRight(s, w)
}

// fitEnd is fit for names, cutting from the end.
func fitEnd(s string, w int) string {
	if w < 1 {
		return ""
	}
	return runewidth.FillRight(runewidth.Truncate(s, w, "…"), w)
}

func bar(frac float64, w int) string {
	full := min(max(int(frac*float64(w)), 0), w)
	return strings.Repeat("█", full) + strings.Repeat("░", w-full)
}

/* View */

var (
	cAccent = lipgloss.Color("39")
	cDim    = lipgloss.Color("240")
	cDir    = lipgloss.Color("75")
	cGood   = lipgloss.Color("42")
	cWarn   = lipgloss.Color("214")
	cBad    = lipgloss.Color("203")
	cUp     = lipgloss.Color("170")

	sDim    = lipgloss.NewStyle().Foreground(cDim)
	sDir    = lipgloss.NewStyle().Foreground(cDir).Bold(true)
	sKey    = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	sTitle  = lipgloss.NewStyle().Bold(true)
	sBrand  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231")).Background(cAccent).Padding(0, 1)
	sSel    = lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Background(lipgloss.Color("25")).Bold(true)
	sSelDim = lipgloss.NewStyle().Background(lipgloss.Color("237"))
	sGood   = lipgloss.NewStyle().Foreground(cGood)
	sWarn   = lipgloss.NewStyle().Foreground(cWarn)
	sBad    = lipgloss.NewStyle().Foreground(cBad)
	sUp     = lipgloss.NewStyle().Foreground(cUp)
)

type layout struct {
	split         bool // both panes side by side; otherwise only the focused one shows
	fileW, queueW int
	paneH         int
	fileRows      int
	queueSlots    int
}

func (m model) layout() layout {
	l := layout{split: m.w >= 80, paneH: max(m.h-3, 5)}
	if l.split {
		l.fileW = m.w * 9 / 20
		l.queueW = m.w - l.fileW
	} else {
		l.fileW, l.queueW = m.w, m.w
	}
	inner := l.paneH - 2
	l.fileRows = max(inner-2, 1) // breadcrumb and a gap above the list
	l.queueSlots = max(inner/2, 1)
	return l
}

// box draws a rounded frame with a title in the top edge. Body lines must already
// be at most w-2 columns wide.
func box(title, info string, body []string, w, h int, focused bool) string {
	border := sDim
	t := sDim.Bold(true)
	if focused {
		border = lipgloss.NewStyle().Foreground(cAccent)
		t = sKey
	}
	iw := max(w-2, 1)
	head := border.Render("╭─ ") + t.Render(title) + " "
	if info != "" && lipgloss.Width(head)+lipgloss.Width(info)+2 <= iw {
		head += sDim.Render(info) + " "
	}
	clip := lipgloss.NewStyle().MaxWidth(iw)
	var out strings.Builder
	out.WriteString(head + border.Render(strings.Repeat("─", max(w-1-lipgloss.Width(head), 0))+"╮") + "\n")
	for i := 0; i < h-2; i++ {
		line := ""
		if i < len(body) {
			line = clip.Render(body[i])
		}
		pad := max(iw-lipgloss.Width(line), 0)
		out.WriteString(border.Render("│") + line + strings.Repeat(" ", pad) + border.Render("│") + "\n")
	}
	out.WriteString(border.Render("╰" + strings.Repeat("─", iw) + "╯"))
	return out.String()
}

func (m model) breadcrumb(w int) string {
	if m.filter != "" || m.prompt == promptFilter {
		label := fmt.Sprintf("filter: %s  (%d of %d)", m.filter, len(m.shown), len(m.entries))
		return " " + sWarn.Render(fitEnd(label, w-1))
	}
	parts := []string{"/"}
	if m.cwd != "" {
		parts = append(parts, strings.Split(m.cwd, "/")...)
	}
	crumb := parts[0] + strings.Join(parts[1:], " › ")
	if len(parts) > 1 {
		crumb = "/ " + strings.Join(parts[1:], " › ")
	}
	return " " + sDim.Render(fit(crumb, w-1))
}

func (m model) rowsBrowser(w, rows int) []string {
	lines := []string{m.breadcrumb(w), ""}
	switch {
	case !m.loaded:
		return append(lines, sDim.Render("  Loading…"))
	case len(m.entries) == 0:
		return append(lines, sDim.Render("  This folder is empty. Drop files here to upload."))
	case len(m.shown) == 0:
		return append(lines, sDim.Render(fmt.Sprintf("  Nothing matches %q. Esc clears the filter.", m.filter)))
	}
	focused := m.pane == filesPane
	start := m.offset[filesPane]
	end := min(start+rows, len(m.shown))
	for i := start; i < end; i++ {
		e := m.shown[i]
		mark := m.marks[path.Join(m.cwd, e.Name)]
		size := bytes_(e.Size)
		name := e.Name
		if e.Dir {
			name += "/"
			size = ""
		}
		nameW := max(w-14, 4)
		if i == m.cursor[filesPane] {
			row := " " + runewidth.FillRight(mark, 1) + " " + fitEnd(name, nameW) + fmt.Sprintf("%10s ", size)
			if focused {
				lines = append(lines, sSel.Render(row))
			} else {
				lines = append(lines, sSelDim.Render(row))
			}
			continue
		}
		gutter := "   "
		switch mark {
		case "↑":
			gutter = " " + sUp.Render(mark) + " "
		case "":
		default:
			gutter = " " + sGood.Render(mark) + " "
		}
		styled := fitEnd(name, nameW)
		if e.Dir {
			styled = sDir.Render(styled)
		}
		lines = append(lines, gutter+styled+sDim.Render(fmt.Sprintf("%10s ", size)))
	}
	return lines
}

// fillTo pads a styled string that holds w columns or fewer up to w.
func fillTo(s string, w int) string {
	return strings.Repeat(" ", max(w-lipgloss.Width(s), 0))
}

func stateStyle(v View) lipgloss.Style {
	switch {
	case v.State == failed:
		return sBad
	case v.State == paused:
		return sWarn
	case v.State == done:
		return sGood
	case v.State == active && v.Err != "":
		return sWarn
	case v.State == active:
		return lipgloss.NewStyle().Foreground(cAccent)
	}
	return sDim
}

func queueInfo(v View) string {
	amount := fmt.Sprintf("%s / %s", bytes_(v.Sent), bytes_(v.Size))
	switch v.State {
	case failed:
		return "failed: " + v.Err + "  (space retries)"
	case done:
		return "done · " + bytes_(v.Size)
	case paused:
		return "paused · " + amount
	case queued:
		return "waiting · " + amount
	}
	if v.Err != "" {
		return v.Err
	}
	if v.Speed > 0 {
		return fmt.Sprintf("%s · %s/s · %s left", amount, bytes_(int64(v.Speed)), eta(v.Size-v.Sent, v.Speed))
	}
	return amount + " · starting"
}

func (m model) rowsQueue(w, slots int) []string {
	if len(m.views) == 0 {
		return []string{
			"",
			sDim.Render(fitEnd("  Nothing queued.", w)),
			"",
			sDim.Render(fitEnd("  d downloads the selected file or folder.", w)),
			sDim.Render(fitEnd("  Drop files on this window to upload them", w)),
			sDim.Render(fitEnd("  into the folder open on the left.", w)),
		}
	}
	lines := []string{}
	focused := m.pane == queuePane
	start := m.offset[queuePane]
	end := min(start+slots, len(m.views))
	for i := start; i < end; i++ {
		v := m.views[i]
		arrow, as := "↓", sGood
		if v.Item.Up {
			arrow, as = "↑", sUp
		}
		frac := 0.0
		if v.Size > 0 {
			frac = float64(v.Sent) / float64(v.Size)
		}
		barW := min(16, max(w/4, 6))
		pct := fmt.Sprintf("%3.0f%%", frac*100)
		name := fit(v.Item.Remote, max(w-4, 1))
		info := fitEnd(queueInfo(v), max(w-barW-11, 1))
		if i == m.cursor[queuePane] {
			sel := sSelDim
			if focused {
				sel = sSel
			}
			lines = append(lines,
				sel.Render(" "+arrow+" "+name+" "),
				sel.Render("   "+bar(frac, barW)+" "+pct+"  "+info+fillTo(info, max(w-barW-11, 1))+" "))
			continue
		}
		st := stateStyle(v)
		nameStyle := lipgloss.NewStyle()
		if v.State == done {
			nameStyle = sDim
		}
		lines = append(lines,
			" "+as.Render(arrow)+" "+nameStyle.Render(name),
			"   "+st.Render(bar(frac, barW))+" "+sDim.Render(pct)+"  "+st.Render(info))
	}
	if more := len(m.views) - end; more > 0 {
		lines = append(lines, sDim.Render(fitEnd(fmt.Sprintf("   … %d more below", more), w)))
	}
	return lines
}

func (m model) queueSummary() string {
	counts := map[string]int{}
	for _, v := range m.views {
		counts[v.State]++
	}
	parts := []string{}
	for _, s := range []string{active, queued, paused, failed, done} {
		if counts[s] > 0 {
			label := s
			if s == queued {
				label = "waiting"
			}
			parts = append(parts, fmt.Sprintf("%d %s", counts[s], label))
		}
	}
	return strings.Join(parts, " · ")
}

func (m model) header() string {
	var down, up float64
	for _, v := range m.views {
		if v.Item.Up {
			up += v.Speed
		} else {
			down += v.Speed
		}
	}
	host := m.e.Server
	if u, err := url.Parse(m.e.Server); err == nil && u.Host != "" {
		host = u.Host
	}
	left := sBrand.Render("Game Share") + " " + sDim.Render(host)
	rates := sGood.Render("↓ "+bytes_(int64(down))+"/s") + "  " + sUp.Render("↑ "+bytes_(int64(up))+"/s")
	right := rates + sDim.Render("  │  ") + bytes_(m.free) + sDim.Render(" free on server  │  saving to ") + m.e.Dest + " "
	if lipgloss.Width(left)+lipgloss.Width(right)+1 > m.w {
		right = rates + sDim.Render("  │  ") + bytes_(m.free) + sDim.Render(" free ")
	}
	if lipgloss.Width(left)+lipgloss.Width(right)+1 > m.w {
		right = rates + " "
	}
	gap := max(m.w-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return left + strings.Repeat(" ", gap) + right
}

type hint struct{ key, desc string }

func (m model) hints() []hint {
	common := []hint{{"tab", "switch"}, {"u", "upload"}, {"q", "quit"}, {"?", "help"}}
	if m.pane == queuePane {
		return append([]hint{{"space", "pause/resume"}, {"n", "next"}, {"x", "remove"}, {"o", "show in files"}, {"c", "clear done"}}, common...)
	}
	return append([]hint{{"enter", "open"}, {"←", "back"}, {"d", "download"}, {"a", "all here"}, {"/", "filter"}, {"x", "delete"}}, common...)
}

// renderHints fits as many hints as the width allows, always keeping the last one
// (help) so the rest can be found.
func renderHints(hs []hint, w int) string {
	piece := func(h hint) string { return sKey.Render(h.key) + " " + sDim.Render(h.desc) + "   " }
	last := piece(hs[len(hs)-1])
	out := " "
	for _, h := range hs[:len(hs)-1] {
		p := piece(h)
		if lipgloss.Width(out)+lipgloss.Width(p)+lipgloss.Width(last) > w {
			break
		}
		out += p
	}
	return out + last
}

func (m model) statusLine() string {
	switch m.prompt {
	case promptUpload:
		return " " + sKey.Render("Upload into /"+m.cwd+":") + " " + m.input + "▌" + sDim.Render("  (type or drop paths, enter to queue, esc to cancel)")
	case promptFilter:
		return " " + sKey.Render("Filter:") + " " + m.input + "▌" + sDim.Render("  (enter keeps it, esc clears)")
	case promptDelete:
		what := "/" + m.target
		for _, e := range m.entries {
			if path.Join(m.cwd, e.Name) == m.target && e.Dir {
				what += "/ and everything in it"
			}
		}
		return " " + sBad.Bold(true).Render("Delete "+what+" from the server?") + " This can't be undone. " +
			sKey.Render("y") + sDim.Render(" delete  ") + sKey.Render("any other key") + sDim.Render(" cancel")
	}
	if m.note == "" {
		return ""
	}
	if m.noteErr {
		return " " + sBad.Render("✗ "+m.note)
	}
	return " " + sGood.Render("✓ ") + m.note
}

func (m model) helpView(w, h int) string {
	rows := [][2]string{
		{"", "Everywhere"},
		{"tab", "switch between files and queue"},
		{"↑ ↓  k j", "move"},
		{"pgup pgdn", "move a page (also ctrl+u, ctrl+d)"},
		{"home end", "jump to the top or bottom (also g, G)"},
		{"u", "type or drop paths to upload into the open folder"},
		{"c", "clear finished items from the queue"},
		{"q", "quit; the queue resumes on the next run"},
		{"", ""},
		{"", "Files"},
		{"enter → l", "open a folder"},
		{"← h bksp", "go up; ~ goes to the top"},
		{"d", "download the selected file or folder"},
		{"a", "download everything in this folder"},
		{"/", "filter this folder by name; esc clears"},
		{"x  del", "delete from the server (asks first)"},
		{"r", "refresh"},
		{"", ""},
		{"", "Queue"},
		{"space p", "pause or resume; retries a failed item"},
		{"n", "run this item next"},
		{"x  del", "remove from the queue; partial data stays, so re-adding resumes"},
		{"o enter", "show the item's folder in the file list"},
	}
	lines := []string{""}
	for _, r := range rows {
		switch {
		case r[0] == "" && r[1] == "":
			lines = append(lines, "")
		case r[0] == "":
			lines = append(lines, "  "+sTitle.Render(r[1]))
		default:
			lines = append(lines, "  "+sKey.Render(runewidth.FillRight(r[0], 11))+" "+runewidth.Truncate(r[1], max(w-17, 1), "…"))
		}
	}
	lines = append(lines, "", "  "+sDim.Render("Press any key to close."))
	if len(lines) > h-2 { // short terminal: drop the spacing first
		compact := []string{}
		for _, l := range lines {
			if l != "" {
				compact = append(compact, l)
			}
		}
		lines = compact
	}
	if len(lines) > h-2 && w >= 2*60 { // still too tall: two columns
		half := (len(lines) + 1) / 2
		colW := (w - 2) / 2
		joined := []string{}
		for i := 0; i < half; i++ {
			clip := lipgloss.NewStyle().MaxWidth(colW)
			l := clip.Render(lines[i])
			l += fillTo(l, colW)
			if i+half < len(lines) {
				l += clip.Render(lines[i+half])
			}
			joined = append(joined, l)
		}
		lines = joined
	}
	return box("Keys", "", lines, w, h, true)
}

func (m model) View() string {
	l := m.layout()
	var body string
	switch {
	case m.help:
		body = m.helpView(m.w, l.paneH)
	default:
		info := ""
		if len(m.shown) > 0 {
			info = fmt.Sprintf("%d/%d", m.cursor[filesPane]+1, len(m.shown))
		}
		summary := m.queueSummary()
		if !l.split { // only one pane fits, so say where the other went
			info = strings.TrimSpace(info + "  tab → Queue")
			summary = strings.TrimSpace(summary + "  tab → Files")
		}
		files := box("Files", info,
			m.rowsBrowser(l.fileW-2, l.fileRows), l.fileW, l.paneH, m.pane == filesPane)
		queue := box("Queue", summary,
			m.rowsQueue(l.queueW-2, l.queueSlots), l.queueW, l.paneH, m.pane == queuePane)
		switch {
		case l.split:
			body = lipgloss.JoinHorizontal(lipgloss.Top, files, queue)
		case m.pane == filesPane:
			body = files
		default:
			body = queue
		}
	}
	clip := lipgloss.NewStyle().MaxWidth(m.w)
	return clip.Render(m.header()) + "\n" + body + "\n" + clip.Render(m.statusLine()) + "\n" + renderHints(m.hints(), m.w)
}
