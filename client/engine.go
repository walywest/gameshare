package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	largeFile  = 256 << 20 // larger items run one at a time per direction
	smallSlots = 4         // small items run in parallel, alongside one large item
	copyBuffer = 4 << 20
	overlap    = 1 << 20          // bytes re-read on resume to prove the part file still matches
	stallAfter = 20 * time.Second // a pulled cable gives no error, only silence
	maxBackoff = 15 * time.Second
)

// Item states. Network trouble never fails an item: it stays active and reconnects.
// Failed means something a retry can't fix alone, such as a full disk.
const (
	queued = "queued"
	active = "active"
	paused = "paused"
	done   = "done"
	failed = "failed"
)

type Item struct {
	ID     int    `json:"id"`
	Up     bool   `json:"up"`
	Remote string `json:"remote"`
	Local  string `json:"local"`
	Size   int64  `json:"size"`
	MTime  int64  `json:"mtime"`
	State  string `json:"state"`
	Err    string `json:"err,omitempty"`
	First  bool   `json:"first,omitempty"`

	sent atomic.Int64
}

func (i *Item) large() bool { return i.Size >= largeFile }

// View is a copy of an Item that is safe to read while transfers run.
type View struct {
	Item  *Item
	Size  int64
	Sent  int64
	Speed float64
	State string
	Err   string
}

type saved struct {
	Server string  `json:"server"`
	NextID int     `json:"next_id"`
	Items  []*Item `json:"items"`
}

type Engine struct {
	Server string
	Dest   string

	mu      sync.Mutex
	items   []*Item
	nextID  int
	cancels map[int]context.CancelFunc
	speeds  map[int]*meter
	state   string
	client  *http.Client
}

type meter struct {
	last  int64
	at    time.Time
	speed float64
}

func NewEngine(server, dest, stateFile string) *Engine {
	return &Engine{
		Server: server, Dest: dest, state: stateFile, nextID: 1,
		cancels: map[int]context.CancelFunc{}, speeds: map[int]*meter{},
		client: &http.Client{Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			DisableCompression:    true,
			ResponseHeaderTimeout: 40 * time.Second, // the server may wait out a dead upload
			WriteBufferSize:       1 << 20,
			ReadBufferSize:        1 << 20,
		}},
	}
}

func loadSaved(file string) saved {
	var s saved
	if data, err := os.ReadFile(file); err == nil {
		json.Unmarshal(data, &s)
	}
	return s
}

// Restore loads the saved queue. Anything interrupted goes back in line.
func (e *Engine) Restore() {
	s := loadSaved(e.state)
	e.mu.Lock()
	defer e.mu.Unlock()
	if s.NextID > e.nextID {
		e.nextID = s.NextID
	}
	for _, it := range s.Items {
		if it.State == active {
			it.State = queued
		}
		it.Err = ""
		if info, err := os.Stat(it.Local + ".part"); err == nil && !it.Up && it.State != done {
			it.sent.Store(info.Size())
		}
		if it.State == done {
			it.sent.Store(it.Size)
		}
		e.items = append(e.items, it)
	}
}

func (e *Engine) Save() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.saveLocked()
}

// saveLocked writes through a temp file so a crash can't leave a torn queue.
func (e *Engine) saveLocked() {
	data, err := json.MarshalIndent(saved{e.Server, e.nextID, e.items}, "", " ")
	if err != nil {
		return
	}
	os.MkdirAll(filepath.Dir(e.state), 0o755)
	tmp := e.state + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		os.Rename(tmp, e.state)
	}
}

func (e *Engine) Add(it *Item) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, o := range e.items {
		if o.Up == it.Up && o.Remote == it.Remote && o.Local == it.Local && o.State != done {
			return
		}
	}
	it.ID, it.State = e.nextID, queued
	e.nextID++
	e.items = append(e.items, it)
	e.scheduleLocked()
}

// Kick starts whatever the restored queue has waiting.
func (e *Engine) Kick() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.scheduleLocked()
}

func (e *Engine) scheduleLocked() {
	waiting := []*Item{}
	running := map[bool][2]int{} // by direction: large, small
	for _, it := range e.items {
		if it.State == queued {
			waiting = append(waiting, it)
		}
		if it.State == active {
			c := running[it.Up]
			if it.large() {
				c[0]++
			} else {
				c[1]++
			}
			running[it.Up] = c
		}
	}
	// Small items first so one huge file doesn't hold up the rest; Next overrides that.
	sort.SliceStable(waiting, func(a, b int) bool {
		if waiting[a].First != waiting[b].First {
			return waiting[a].First
		}
		return waiting[a].Size < waiting[b].Size
	})
	for _, it := range waiting {
		c := running[it.Up]
		if it.large() && c[0] == 0 {
			c[0]++
		} else if !it.large() && c[1] < smallSlots {
			c[1]++
		} else {
			continue
		}
		running[it.Up] = c
		it.State, it.Err = active, ""
		ctx, cancel := context.WithCancel(context.Background())
		e.cancels[it.ID] = cancel
		go e.run(ctx, it)
	}
	e.saveLocked()
}

// hardError stops an item instead of reconnecting.
type hardError struct{ msg string }

func (h hardError) Error() string { return h.msg }

// run retries until the transfer finishes, fails for good, or is paused.
func (e *Engine) run(ctx context.Context, it *Item) {
	wait := time.Second
	for {
		before := it.sent.Load()
		var err error
		if it.Up {
			err = e.upload(ctx, it)
		} else {
			err = e.download(ctx, it)
		}
		var hard hardError
		switch {
		case ctx.Err() != nil:
			return // paused or removed: whoever cancelled set the state
		case err == nil:
			e.finish(it, done, "")
			return
		case errors.As(err, &hard):
			e.finish(it, failed, hard.msg)
			return
		}
		if it.sent.Load() > before {
			wait = time.Second
		}
		e.setErr(it, "reconnecting: "+shortErr(err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait *= 2; wait > maxBackoff {
			wait = maxBackoff
		}
	}
}

func shortErr(err error) string {
	var op *net.OpError
	if errors.As(err, &op) {
		return op.Err.Error()
	}
	var u *url.Error
	if errors.As(err, &u) {
		return u.Err.Error()
	}
	return err.Error()
}

func (e *Engine) setErr(it *Item, msg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	it.Err = msg
}

func (e *Engine) finish(it *Item, state, msg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cancel := e.cancels[it.ID]; cancel != nil {
		cancel()
		delete(e.cancels, it.ID)
	}
	it.State, it.Err = state, msg
	if state == done {
		it.sent.Store(it.Size)
	}
	e.scheduleLocked()
}

func (e *Engine) find(id int) *Item {
	for _, it := range e.items {
		if it.ID == id {
			return it
		}
	}
	return nil
}

// Toggle pauses an active or queued item and resumes a paused or failed one.
func (e *Engine) Toggle(id int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	it := e.find(id)
	if it == nil {
		return
	}
	switch it.State {
	case active, queued:
		if cancel := e.cancels[id]; cancel != nil {
			cancel()
			delete(e.cancels, id)
		}
		it.State, it.Err = paused, ""
	case paused, failed:
		it.State, it.Err = queued, ""
	}
	e.scheduleLocked()
}

func (e *Engine) Next(id int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if it := e.find(id); it != nil {
		it.First = true
		e.scheduleLocked()
	}
}

// Remove drops an item from the queue. Its part file stays, so queueing it again resumes.
func (e *Engine) Remove(id int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cancel := e.cancels[id]; cancel != nil {
		cancel()
		delete(e.cancels, id)
	}
	for i, it := range e.items {
		if it.ID == id {
			e.items = append(e.items[:i], e.items[i+1:]...)
			break
		}
	}
	e.scheduleLocked()
}

func (e *Engine) ClearDone() {
	e.mu.Lock()
	defer e.mu.Unlock()
	kept := e.items[:0]
	for _, it := range e.items {
		if it.State != done {
			kept = append(kept, it)
		}
	}
	e.items = kept
	e.saveLocked()
}

// Pending counts items that aren't finished or stopped.
func (e *Engine) Pending() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, it := range e.items {
		if it.State == queued || it.State == active {
			n++
		}
	}
	return n
}

// Snapshot returns the queue in display order and updates the speed meters.
func (e *Engine) Snapshot() []View {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	views := make([]View, 0, len(e.items))
	for _, it := range e.items {
		v := View{Item: it, Size: it.Size, Sent: it.sent.Load(), State: it.State, Err: it.Err}
		if it.State == active {
			m := e.speeds[it.ID]
			if m == nil {
				m = &meter{last: v.Sent, at: now}
				e.speeds[it.ID] = m
			}
			if dt := now.Sub(m.at).Seconds(); dt >= 0.4 {
				rate := float64(max(v.Sent-m.last, 0)) / dt
				if m.speed == 0 {
					m.speed = rate
				}
				m.speed = m.speed*0.7 + rate*0.3
				m.last, m.at = v.Sent, now
			}
			v.Speed = m.speed
		} else {
			delete(e.speeds, it.ID)
		}
		views = append(views, v)
	}
	rank := map[string]int{active: 0, queued: 1, paused: 2, failed: 3, done: 4}
	sort.SliceStable(views, func(a, b int) bool { return rank[views[a].State] < rank[views[b].State] })
	return views
}

// guard cancels a transfer when no bytes move for stallAfter.
func guard(ctx context.Context) (context.Context, func(), func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	stalled := errors.New("no data for 20s")
	timer := time.AfterFunc(stallAfter, func() { cancel(stalled) })
	return ctx, func() { timer.Reset(stallAfter) }, func() { timer.Stop(); cancel(nil) }
}

type tickReader struct {
	r    io.Reader
	tick func()
}

func (t tickReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.tick()
	}
	return n, err
}

func (e *Engine) url(route string, q url.Values) string {
	return e.Server + route + "?" + q.Encode()
}

func etag(it *Item) string { return fmt.Sprintf(`"%d-%d"`, it.Size, it.MTime) }

func sameFile(p string, size, mtime int64) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir() && info.Size() == size && info.ModTime().UnixMilli() == mtime
}

func (e *Engine) download(parent context.Context, it *Item) error {
	if sameFile(it.Local, it.Size, it.MTime) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(it.Local), 0o755); err != nil {
		return hardError{err.Error()}
	}
	part := it.Local + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return hardError{err.Error()}
	}
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return hardError{err.Error()}
	}
	offset := info.Size()
	it.sent.Store(offset)

	ctx, tick, stop := guard(parent)
	defer stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.url("/dl", url.Values{"path": {it.Remote}}), nil)
	if err != nil {
		return hardError{err.Error()}
	}
	start := max(offset-overlap, 0)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
		req.Header.Set("If-Range", etag(it))
	}
	resp, err := e.client.Do(req)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		return err
	}
	defer resp.Body.Close()
	body := tickReader{resp.Body, tick}

	switch resp.StatusCode {
	case http.StatusPartialContent:
		if err := checkOverlap(f, body, start, offset); err != nil {
			if errors.Is(err, errMismatch) {
				f.Truncate(0)
				it.sent.Store(0)
			}
			return err
		}
	case http.StatusOK:
		// First request, or the server file changed and If-Range refused the range.
		offset = 0
		it.sent.Store(0)
		if err := f.Truncate(0); err != nil {
			return hardError{err.Error()}
		}
		e.adopt(it, resp)
	case http.StatusRequestedRangeNotSatisfiable:
		f.Truncate(0)
		return errors.New("part file larger than the server's")
	case http.StatusNotFound:
		return hardError{"not on the server any more"}
	default:
		return fmt.Errorf("server said %s", resp.Status)
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return hardError{err.Error()}
	}
	buf := make([]byte, copyBuffer)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return hardError{"can't write: " + werr.Error()}
			}
			it.sent.Add(int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			return rerr
		}
	}
	if got := it.sent.Load(); got != it.Size {
		return fmt.Errorf("stream ended at %d of %d", got, it.Size)
	}
	if err := f.Sync(); err != nil {
		return hardError{err.Error()}
	}
	f.Close()
	f = nil
	mtime := time.UnixMilli(it.MTime)
	if err := os.Chtimes(part, mtime, mtime); err != nil {
		return hardError{err.Error()}
	}
	if err := os.Rename(part, it.Local); err != nil {
		return hardError{err.Error()}
	}
	return nil
}

var errMismatch = errors.New("part file doesn't match the server, restarting it")

// checkOverlap compares the bytes both sides should share, before appending to them.
func checkOverlap(f *os.File, body io.Reader, start, offset int64) error {
	n := offset - start
	theirs := make([]byte, n)
	if _, err := io.ReadFull(body, theirs); err != nil {
		return err
	}
	ours := make([]byte, n)
	if _, err := f.ReadAt(ours, start); err != nil {
		return hardError{err.Error()}
	}
	if !bytes.Equal(ours, theirs) {
		return errMismatch
	}
	return nil
}

// adopt takes size and mtime from the ETag, whose millisecond mtime has to stay exact
// for later If-Range checks. Last-Modified only has whole seconds.
func (e *Engine) adopt(it *Item, resp *http.Response) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var size, mtime int64
	if n, _ := fmt.Sscanf(resp.Header.Get("ETag"), `"%d-%d"`, &size, &mtime); n == 2 {
		it.Size, it.MTime = size, mtime
	}
	e.saveLocked()
}

func (e *Engine) upload(parent context.Context, it *Item) error {
	f, err := os.Open(it.Local)
	if err != nil {
		return hardError{err.Error()}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return hardError{err.Error()}
	}
	if info.Size() != it.Size || info.ModTime().UnixMilli() != it.MTime {
		e.mu.Lock()
		it.Size, it.MTime = info.Size(), info.ModTime().UnixMilli()
		e.saveLocked()
		e.mu.Unlock()
	}
	q := url.Values{"path": {it.Remote}, "size": {fmt.Sprint(it.Size)}, "mtime": {fmt.Sprint(it.MTime)}}

	ctx, tick, stop := guard(parent)
	defer stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.url("/api/upload", q), nil)
	if err != nil {
		return hardError{err.Error()}
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	var status struct {
		Offset   int64 `json:"offset"`
		Complete bool  `json:"complete"`
	}
	err = json.NewDecoder(resp.Body).Decode(&status)
	code := resp.StatusCode
	resp.Body.Close()
	if code != http.StatusOK {
		return hardError{fmt.Sprintf("server refused it (%d)", code)}
	}
	if err != nil {
		return err
	}
	if status.Complete {
		return nil
	}
	if status.Offset > it.Size {
		return hardError{"server has a larger partial upload"}
	}
	it.sent.Store(status.Offset)
	tick()

	q.Set("offset", fmt.Sprint(status.Offset))
	remaining := it.Size - status.Offset
	var body io.Reader = http.NoBody
	if remaining > 0 {
		section := io.NewSectionReader(f, status.Offset, remaining)
		// Large reads matter on /mnt: WSL's 9p bridge is far slower with small ones.
		body = tickReader{countReader{bufio.NewReaderSize(section, copyBuffer), &it.sent}, tick}
	}
	put, err := http.NewRequestWithContext(ctx, http.MethodPut, e.url("/api/upload", q), io.NopCloser(body))
	if err != nil {
		return hardError{err.Error()}
	}
	if remaining == 0 {
		put.Body = http.NoBody
	}
	put.ContentLength = remaining
	presp, err := e.client.Do(put)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		return err
	}
	defer presp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(presp.Body, 200))
	switch presp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusInsufficientStorage:
		return hardError{"server is out of disk space"}
	default:
		return fmt.Errorf("server said %s: %s", presp.Status, bytes.TrimSpace(msg))
	}
}

type countReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

/* Listing and queueing */

type Entry struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
}

func (e *Engine) getJSON(route string, q url.Values, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.url(route, q), nil)
	if err != nil {
		return err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (e *Engine) List(dir string) ([]Entry, int64, error) {
	var out struct {
		Entries []Entry `json:"entries"`
		Free    int64   `json:"free"`
	}
	err := e.getJSON("/api/list", url.Values{"path": {dir}}, &out)
	return out.Entries, out.Free, err
}

// QueueDownload adds a remote file, or every file in a remote folder, keeping the
// layout below base (the folder being browsed).
func (e *Engine) QueueDownload(base, remote string) (int, error) {
	var files []Entry
	if err := e.getJSON("/api/tree", url.Values{"path": {remote}}, &files); err != nil {
		return 0, err
	}
	for _, f := range files {
		rel := f.Name
		if base != "" {
			rel = f.Name[len(path.Clean(base))+1:]
		}
		if !filepath.IsLocal(filepath.FromSlash(rel)) {
			continue // never write outside the destination, whatever a server sends
		}
		e.Add(&Item{Remote: f.Name, Local: filepath.Join(e.Dest, filepath.FromSlash(rel)), Size: f.Size, MTime: f.MTime})
	}
	return len(files), nil
}

// QueueUpload adds a local file, or every file in a local folder, under remoteDir.
func (e *Engine) QueueUpload(local, remoteDir string) (int, error) {
	local = filepath.Clean(local)
	parent := filepath.Dir(local)
	count := 0
	err := filepath.WalkDir(local, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(parent, p)
		if err != nil {
			return err
		}
		e.Add(&Item{Up: true, Remote: path.Join(remoteDir, filepath.ToSlash(rel)), Local: p, Size: info.Size(), MTime: info.ModTime().UnixMilli()})
		count++
		return nil
	})
	return count, err
}

// Delete removes a remote file or folder from the server, then drops any unfinished
// downloads of it from the queue, since they could only fail now.
func (e *Engine) Delete(remote string) error {
	remote = path.Clean(strings.Trim(remote, "/"))
	if remote == "." || remote == "" {
		return errors.New("refusing to delete the whole share")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, e.url("/api/delete", url.Values{"path": {remote}}), nil)
	if err != nil {
		return err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return errors.New(shortErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		if len(bytes.TrimSpace(msg)) == 0 {
			return errors.New(resp.Status)
		}
		return errors.New(string(bytes.TrimSpace(msg)))
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	kept := e.items[:0]
	for _, it := range e.items {
		under := it.Remote == remote || strings.HasPrefix(it.Remote, remote+"/")
		if under && !it.Up && it.State != done {
			if c := e.cancels[it.ID]; c != nil {
				c()
				delete(e.cancels, it.ID)
			}
			continue
		}
		kept = append(kept, it)
	}
	e.items = kept
	e.scheduleLocked()
	return nil
}

// UpdateResult is the server's reply to an update request.
type UpdateResult struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Log        string `json:"log"`
	ServerCode bool   `json:"serverCode"`
	BuildErr   string `json:"buildErr"`
	Restarting bool   `json:"restarting"`
}

// UpdateServer asks a server in CD mode to pull its code from GitHub and restart if
// that changed it. Running transfers reconnect after the restart by themselves.
func (e *Engine) UpdateServer() (UpdateResult, error) {
	var res UpdateResult
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url("/api/update", nil), nil)
	if err != nil {
		return res, err
	}
	// Not e.client: pulling and building can outlast its header timeout.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return res, errors.New(shortErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return res, errors.New("the server isn't in CD mode; start it with serve --cd")
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		if len(bytes.TrimSpace(msg)) == 0 {
			return res, errors.New(resp.Status)
		}
		return res, errors.New(string(bytes.TrimSpace(msg)))
	}
	return res, json.NewDecoder(resp.Body).Decode(&res)
}

// Summary says in one line what an update did.
func (r UpdateResult) Summary() (string, bool) {
	commits := 0
	if r.Log != "" {
		commits = len(strings.Split(r.Log, "\n"))
	}
	pulled := fmt.Sprintf("Pulled %s → %s (%d new commit", r.From, r.To, commits)
	if commits != 1 {
		pulled += "s"
	}
	pulled += ")"
	switch {
	case r.From == r.To:
		return "Server already up to date at " + r.To, false
	case r.BuildErr != "":
		first, _, _ := strings.Cut(r.BuildErr, "\n")
		return pulled + ", but it doesn't build, so the old server keeps running: " + first, true
	case r.Restarting:
		return pulled + ". Server restarting; transfers resume by themselves", false
	default:
		return pulled + ". No server code changed, so no restart", false
	}
}
