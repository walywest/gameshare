package main

import (
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Storage and live transfers, for the homelab page. Both are read-only views:
// nothing here sits on the copy path, so they can't slow a transfer down.

// ── transfers ────────────────────────────────────────────────────────────────

// transfer is one upload or download in progress. pos reports how far it has
// got, from the file itself, so the copy loop doesn't have to count bytes.
type transfer struct {
	Kind   string    `json:"kind"` // "upload" or "download"
	Path   string    `json:"path"` // relative to the share
	Client string    `json:"client"`
	Size   int64     `json:"size"`
	Start  int64     `json:"start"` // offset it resumed from
	Done   int64     `json:"done"`
	Since  time.Time `json:"since"`
	pos    func() int64
}

var (
	transfersMu sync.Mutex
	transfers   = map[*transfer]struct{}{}
)

// track registers a transfer and returns the function that ends it.
func track(t *transfer) func() {
	t.Since = time.Now()
	transfersMu.Lock()
	transfers[t] = struct{}{}
	transfersMu.Unlock()
	return func() {
		transfersMu.Lock()
		delete(transfers, t)
		transfersMu.Unlock()
	}
}

func shareRel(abs string) string {
	if rel, err := filepath.Rel(root, abs); err == nil {
		return filepath.ToSlash(rel)
	}
	return filepath.Base(abs)
}

func clientOf(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// filePos is where reads (and sendfile, which advances the offset too) have got to.
func filePos(f *os.File) func() int64 {
	return func() int64 {
		n, _ := f.Seek(0, io.SeekCurrent)
		return n
	}
}

func handleTransfers(w http.ResponseWriter, _ *http.Request) {
	transfersMu.Lock()
	list := make([]transfer, 0, len(transfers))
	for t := range transfers {
		c := *t
		if t.pos != nil {
			c.Done = t.pos()
		}
		list = append(list, c)
	}
	transfersMu.Unlock()
	sort.Slice(list, func(a, b int) bool { return list[a].Since.Before(list[b].Since) })
	writeJSON(w, map[string]any{"transfers": list, "now": time.Now()})
}

// ── storage ──────────────────────────────────────────────────────────────────

type folderUse struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	Files int    `json:"files"`
}

type storageView struct {
	Root     string       `json:"root"`
	Total    int64        `json:"total"`    // the disk the share is on
	Free     int64        `json:"free"`     // what we can still write
	Used     int64        `json:"used"`     // by everything on the disk
	Share    int64        `json:"share"`    // by the shared files
	Folders  []folderUse  `json:"folders"`  // top level of the share, largest first
	Partials int64        `json:"partials"` // unfinished uploads, kept for resuming
	PartN    int          `json:"partN"`
	Archives []archiveDir `json:"archives"` // folders holding RAR volumes, largest first
	Scanned  time.Time    `json:"scanned"`
}

// archiveDir is a folder holding RAR volumes, and what sits beside them: the
// extracted files, meaning everything under it that isn't part of the download
// (see extractedFile).
type archiveDir struct {
	Name           string `json:"name"`
	Files          int    `json:"files"`
	Size           int64  `json:"size"`
	ExtractedFiles int    `json:"extractedFiles"`
	Extracted      int64  `json:"extracted"`
}

var (
	storageMu    sync.Mutex
	storageCache *storageView
)

// A walk of a few hundred game files is quick, but the page polls, so it's cached.
const storageTTL = 15 * time.Second

// diskUse fills in the disk numbers. They're cheap, so they're never cached.
func (v *storageView) diskUse() {
	var st syscall.Statfs_t
	if syscall.Statfs(root, &st) == nil {
		v.Total = int64(st.Blocks) * st.Bsize
		v.Free = int64(st.Bavail) * st.Bsize
		v.Used = int64(st.Blocks-st.Bfree) * st.Bsize
	}
}

func scanStorage() *storageView {
	v := &storageView{Root: root, Folders: []folderUse{}, Archives: []archiveDir{}, Scanned: time.Now()}
	archives := map[string]*archiveDir{}
	plain := map[string]*archiveDir{} // per folder, files that could be extracted ones
	items, _ := os.ReadDir(root)
	for _, it := range items {
		if strings.HasPrefix(it.Name(), ".") {
			continue
		}
		f := folderUse{Name: it.Name(), Dir: it.IsDir()}
		filepath.WalkDir(filepath.Join(root, it.Name()), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable bits are skipped, not fatal
			}
			if d.IsDir() {
				if strings.HasPrefix(d.Name(), ".") && d.Name() != it.Name() {
					return fs.SkipDir
				}
				return nil
			}
			if info, err := d.Info(); err == nil && d.Type().IsRegular() {
				f.Size += info.Size()
				f.Files++
				dir := shareRel(filepath.Dir(p))
				if dir == "." {
					dir = ""
				}
				add := func(m map[string]*archiveDir) {
					a := m[dir]
					if a == nil {
						a = &archiveDir{Name: dir}
						m[dir] = a
					}
					a.Size += info.Size()
					a.Files++
				}
				if isArchive(d.Name()) && !strings.HasPrefix(d.Name(), ".") {
					add(archives)
				} else if extractedFile(d.Name()) {
					add(plain)
				}
			}
			return nil
		})
		v.Share += f.Size
		v.Folders = append(v.Folders, f)
	}
	sort.Slice(v.Folders, func(a, b int) bool { return v.Folders[a].Size > v.Folders[b].Size })
	for _, a := range archives {
		for d, p := range plain {
			if a.Name == "" || d == a.Name || strings.HasPrefix(d, a.Name+"/") {
				a.Extracted += p.Size
				a.ExtractedFiles += p.Files
			}
		}
		v.Archives = append(v.Archives, *a)
	}
	sort.Slice(v.Archives, func(a, b int) bool { return v.Archives[a].Size > v.Archives[b].Size })
	if parts, err := os.ReadDir(partDir); err == nil {
		for _, p := range parts {
			if info, err := p.Info(); err == nil && strings.HasSuffix(p.Name(), ".part") {
				v.Partials += info.Size()
				v.PartN++
			}
		}
	}
	return v
}

func handleStorage(w http.ResponseWriter, r *http.Request) {
	storageMu.Lock()
	if storageCache == nil || time.Since(storageCache.Scanned) > storageTTL || r.URL.Query().Get("fresh") != "" {
		storageCache = scanStorage()
	}
	v := *storageCache
	storageMu.Unlock()
	v.diskUse()
	writeJSON(w, v)
}
