// game-share server: a small HTTP API for the gs client, with resumable transfers.
//
// Plain HTTP and no auth on purpose. Paths are still confined to the served folder.
package main

import (
	crand "crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	copyBuffer = 4 << 20
	// A dead client must not hold an upload's lock, or its resume would be refused.
	idleTimeout = 30 * time.Second
)

var (
	root    string
	partDir string

	uploadsMu sync.Mutex
	uploads   = map[string]string{} // part file -> target, for each PUT in progress
)

// resolve maps a request path into the served folder. Dot segments are refused,
// which also keeps .uploads out of reach.
func resolve(rel string) (string, error) {
	clean := path.Clean("/" + rel)
	for _, seg := range strings.Split(clean, "/") {
		if strings.HasPrefix(seg, ".") {
			return "", errors.New("bad path")
		}
	}
	return filepath.Join(root, filepath.FromSlash(clean)), nil
}

func freeBytes() int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * st.Bsize
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

type entry struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
}

func handleList(w http.ResponseWriter, r *http.Request) {
	dir, err := resolve(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	entries := []entry{}
	for _, it := range items {
		info, err := it.Info()
		if err != nil || strings.HasPrefix(it.Name(), ".") {
			continue
		}
		size := info.Size()
		if it.IsDir() {
			size = 0
		}
		entries = append(entries, entry{it.Name(), it.IsDir(), size, info.ModTime().UnixMilli()})
	}
	sort.Slice(entries, func(a, b int) bool {
		if entries[a].Dir != entries[b].Dir {
			return entries[a].Dir
		}
		return strings.ToLower(entries[a].Name) < strings.ToLower(entries[b].Name)
	})
	writeJSON(w, map[string]any{"entries": entries, "free": freeBytes()})
}

// handleTree lists every file under a path, so the client can queue a folder file by
// file and resume each one on its own. Name is the path from the served root.
func handleTree(w http.ResponseWriter, r *http.Request) {
	start, err := resolve(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	files := []entry{}
	err = filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".") && p != root {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files = append(files, entry{Name: filepath.ToSlash(rel), Size: info.Size(), MTime: info.ModTime().UnixMilli()})
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, files)
}

// upload identifies a file by path, size and mtime, so a retry continues its part
// file while a changed source starts a new one.
type upload struct {
	target string
	part   string
	size   int64
	mtime  int64
}

func parseUpload(r *http.Request) (*upload, error) {
	q := r.URL.Query()
	target, err := resolve(q.Get("path"))
	if err != nil || target == root {
		return nil, errors.New("bad path")
	}
	size, err := strconv.ParseInt(q.Get("size"), 10, 64)
	if err != nil || size < 0 {
		return nil, errors.New("bad size")
	}
	mtime, err := strconv.ParseInt(q.Get("mtime"), 10, 64)
	if err != nil {
		return nil, errors.New("bad mtime")
	}
	sum := sha1.Sum([]byte(fmt.Sprintf("%s|%d|%d", target, size, mtime)))
	return &upload{target, filepath.Join(partDir, hex.EncodeToString(sum[:])+".part"), size, mtime}, nil
}

func (u *upload) complete() bool {
	info, err := os.Stat(u.target)
	return err == nil && !info.IsDir() && info.Size() == u.size && info.ModTime().UnixMilli() == u.mtime
}

func (u *upload) offset() int64 {
	if info, err := os.Stat(u.part); err == nil {
		return info.Size()
	}
	return 0
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	u, err := parseUpload(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		waitForWriter(u.part)
		writeJSON(w, map[string]any{"offset": u.offset(), "complete": u.complete()})
	case http.MethodPut:
		putUpload(w, r, u)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// waitForWriter lets a dropped PUT finish flushing, so a resume reads the final offset.
func waitForWriter(part string) {
	for deadline := time.Now().Add(idleTimeout + 5*time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		uploadsMu.Lock()
		_, busy := uploads[part]
		uploadsMu.Unlock()
		if !busy {
			return
		}
	}
}

// idleReader fails a read that sees no data for idleTimeout.
type idleReader struct {
	r  io.Reader
	rc *http.ResponseController
}

func (i idleReader) Read(p []byte) (int, error) {
	i.rc.SetReadDeadline(time.Now().Add(idleTimeout))
	return i.r.Read(p)
}

func putUpload(w http.ResponseWriter, r *http.Request, u *upload) {
	uploadsMu.Lock()
	if _, busy := uploads[u.part]; busy {
		uploadsMu.Unlock()
		http.Error(w, "already uploading", http.StatusConflict)
		return
	}
	uploads[u.part] = u.target
	uploadsMu.Unlock()
	defer func() {
		uploadsMu.Lock()
		delete(uploads, u.part)
		uploadsMu.Unlock()
	}()

	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset != u.offset() {
		log.Printf("upload %s: offset %d, part has %d", u.target, offset, u.offset())
		http.Error(w, "offset mismatch", http.StatusConflict)
		return
	}
	if freeBytes() < u.size-offset {
		http.Error(w, "not enough disk space", http.StatusInsufficientStorage)
		return
	}

	f, err := os.OpenFile(u.part, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Bytes written before a dropped connection stay in the part file for the resume.
	body := idleReader{io.LimitReader(r.Body, u.size-offset), http.NewResponseController(w)}
	_, copyErr := io.CopyBuffer(struct{ io.Writer }{f}, body, make([]byte, copyBuffer))
	closeErr := f.Close()
	http.NewResponseController(w).SetReadDeadline(time.Time{})
	if copyErr != nil || closeErr != nil {
		log.Printf("upload %s: interrupted at %d of %d: %v %v", u.target, u.offset(), u.size, copyErr, closeErr)
		http.Error(w, "upload interrupted", http.StatusBadRequest)
		return
	}
	if u.offset() != u.size {
		http.Error(w, "upload incomplete", http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(filepath.Dir(u.target), 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	mtime := time.UnixMilli(u.mtime)
	if err := os.Chtimes(u.part, mtime, mtime); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.Rename(u.part, u.target); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleDelete removes a file or a whole folder. It refuses the served root, and
// anything an upload is still writing into, since the upload would just recreate it.
func handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	target, err := resolve(r.URL.Query().Get("path"))
	if err != nil || target == root {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	if _, err := os.Lstat(target); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	uploadsMu.Lock()
	defer uploadsMu.Unlock() // held so no upload can start into it meanwhile
	for _, t := range uploads {
		if t == target || strings.HasPrefix(t, target+string(filepath.Separator)) {
			http.Error(w, "an upload is in progress there", http.StatusConflict)
			return
		}
	}
	if err := os.RemoveAll(target); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("deleted %s", target)
	writeJSON(w, map[string]any{"ok": true})
}

// handleDownload serves one file. ServeContent handles Range and If-Range; the ETag
// lets a resuming client notice that the file changed. The client computes the same
// value from the listing, so keep the two in step.
func handleDownload(w http.ResponseWriter, r *http.Request) {
	target, err := resolve(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, err := os.Open(target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.Error(w, "not a file", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": info.Name()}))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", fmt.Sprintf(`"%d-%d"`, info.Size(), info.ModTime().UnixMilli()))
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// handleSpeedDown streams memory for a set time, so it measures only the network.
func handleSpeedDown(w http.ResponseWriter, r *http.Request) {
	ms, err := strconv.Atoi(r.URL.Query().Get("ms"))
	if err != nil || ms < 1 || ms > 10000 {
		http.Error(w, "bad ms", http.StatusBadRequest)
		return
	}
	buf := make([]byte, copyBuffer)
	crand.Read(buf)
	for end := time.Now().Add(time.Duration(ms) * time.Millisecond); time.Now().Before(end); {
		if _, err := w.Write(buf); err != nil {
			return
		}
	}
}

func handleSpeedUp(w http.ResponseWriter, r *http.Request) {
	io.CopyBuffer(struct{ io.Writer }{io.Discard}, struct{ io.Reader }{r.Body}, make([]byte, copyBuffer))
}

// handleSpeedDisk times a flushed write and a read with the page cache dropped, in
// the partial-upload folder so it tests the disk files really land on.
func handleSpeedDisk(w http.ResponseWriter, r *http.Request) {
	read, write, err := diskSpeed()
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"read": read, "write": write})
}

func diskSpeed() (read, write float64, err error) {
	const limit, window, mb = 1 << 30, time.Second, 1 << 20
	if freeBytes() < 2*limit {
		return 0, 0, errors.New("less than 2 GB free")
	}
	f, err := os.CreateTemp(partDir, "speed-*")
	if err != nil {
		return 0, 0, err
	}
	defer os.Remove(f.Name())
	defer f.Close()

	buf := make([]byte, copyBuffer)
	crand.Read(buf)
	start := time.Now()
	var written int64
	for written < limit && time.Since(start) < window {
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
	for time.Since(start) < window {
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

func main() {
	if os.Getenv("GAMESHARE_ASKPASS") == "1" { // git asking for credentials, see update.go
		if len(os.Args) > 1 {
			fmt.Println(askpass(os.Args[1]))
		}
		return
	}
	if len(os.Args) != 3 {
		log.Fatalf("usage: %s <port> <folder>", os.Args[0])
	}
	var err error
	if root, err = filepath.Abs(os.Args[2]); err != nil {
		log.Fatal(err)
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		log.Fatal(err)
	}
	partDir = filepath.Join(root, ".uploads") // same disk, so finishing is a rename
	if err := os.MkdirAll(partDir, 0o755); err != nil {
		log.Fatal(err)
	}

	preflight()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/list", handleList)
	mux.HandleFunc("/api/tree", handleTree)
	mux.HandleFunc("/api/upload", handleUpload)
	mux.HandleFunc("/api/delete", handleDelete)
	mux.HandleFunc("/dl", handleDownload)
	mux.HandleFunc("/api/speed/ping", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("/api/speed/down", handleSpeedDown)
	mux.HandleFunc("/api/speed/up", handleSpeedUp)
	mux.HandleFunc("/api/speed/disk", handleSpeedDisk)
	if cdRepo = os.Getenv("GAMESHARE_CD"); cdRepo != "" {
		if g := os.Getenv("GAMESHARE_GO"); g != "" {
			goBin = g
		}
		mux.HandleFunc("/api/update", handleUpdate)
		log.Printf("CD mode: gs can update this server from %s", cdRepo)
	}

	log.Printf("serving %s on :%s", root, os.Args[1])
	log.Fatal(http.ListenAndServe(":"+os.Args[1], mux))
}
