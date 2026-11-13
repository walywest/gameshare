package main

import (
	"archive/zip"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
)

// Whole-folder actions for the homelab page: download a folder as a zip, and
// clear out the RAR volumes a game leaves behind once it's extracted.

// ── folder download ──────────────────────────────────────────────────────────

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// handleZip streams a folder as a zip. Files are stored, not compressed: game
// files are compressed already, and the network is the bottleneck. The zip is
// built on the fly, so it can't resume after a drop; gs stays the tool for that.
// Dotfiles are left out, as everywhere else. With skip=archives it leaves out the
// RAR download too, which leaves just the extracted files.
func handleZip(w http.ResponseWriter, r *http.Request) {
	extractedOnly := r.URL.Query().Get("skip") == "archives"
	dir, err := resolve(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		http.Error(w, "not a folder", http.StatusNotFound)
		return
	}
	if p := serviceBusy(dir); p != "" {
		http.Error(w, "a service is still downloading into "+p+"; wait until it's done", http.StatusConflict)
		return
	}
	if r.Method == http.MethodHead { // the page checks first, so a refusal isn't saved as a file
		w.Header().Set("Content-Type", "application/zip")
		return
	}

	type item struct {
		abs, name string
		info      fs.FileInfo
	}
	base := filepath.Base(dir)
	var items []item
	var total int64
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dir && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !d.IsDir() && !info.Mode().IsRegular() {
			return nil // symlinks and devices stay out
		}
		if !d.IsDir() && extractedOnly && !extractedFile(d.Name()) {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		name := filepath.ToSlash(filepath.Join(base, rel))
		if d.IsDir() {
			name += "/"
		} else {
			total += info.Size()
		}
		items = append(items, item{p, name, info})
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": base + ".zip"}))
	var sent atomic.Int64
	label := " (zip)"
	if extractedOnly {
		label = " (extracted files, zip)"
	}
	defer track(&transfer{Kind: "download", Path: shareRel(dir) + label, Client: clientOf(r), Size: total, pos: sent.Load})()

	// Once the body has started, a failure can only cut the connection, so the
	// browser marks the download failed instead of saving a truncated zip.
	abort := func(what string, err error) {
		log.Printf("zip %s: %s: %v", dir, what, err)
		panic(http.ErrAbortHandler)
	}
	zw := zip.NewWriter(w)
	buf := make([]byte, copyBuffer)
	for _, it := range items {
		h, err := zip.FileInfoHeader(it.info)
		if err != nil {
			abort(it.name, err)
		}
		h.Name, h.Method = it.name, zip.Store
		fw, err := zw.CreateHeader(h)
		if err != nil {
			abort(it.name, err)
		}
		if it.info.IsDir() {
			continue
		}
		f, err := os.Open(it.abs)
		if err != nil {
			abort(it.name, err)
		}
		_, err = io.CopyBuffer(fw, countingReader{f, &sent}, buf)
		f.Close()
		if err != nil {
			abort(it.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		abort("finishing", err)
	}
}

// ── archive cleanup ──────────────────────────────────────────────────────────

// RAR volumes: name.rar, name.partNN.rar, and old-style name.r00, name.r01...
var archiveName = regexp.MustCompile(`(?i)\.(rar|r\d{2,3})$`)

func isArchive(name string) bool { return archiveName.MatchString(name) }

// extractedFile reports whether a file beside RAR volumes counts as extracted:
// anything but a volume, aria2's resume file for one, or a dotfile such as
// FitFast's .fitfast.json, which is what lets a download resume correctly.
func extractedFile(name string) bool {
	return !strings.HasPrefix(name, ".") && !isArchive(strings.TrimSuffix(name, ".aria2"))
}

// directArchives reports whether dir itself holds a RAR volume.
func directArchives(dir string) bool {
	items, _ := os.ReadDir(dir)
	for _, it := range items {
		if it.Type().IsRegular() && !strings.HasPrefix(it.Name(), ".") && isArchive(it.Name()) {
			return true
		}
	}
	return false
}

// handleDeleteExtracted deletes what was extracted beside a folder's RAR volumes
// and keeps the download: the volumes, their aria2 files, and dotfiles. It only
// takes a folder that holds volumes itself. Without them nothing tells extracted
// files from the game, and this would delete the game.
func handleDeleteExtracted(w http.ResponseWriter, r *http.Request) {
	dir, err := resolve(r.URL.Query().Get("path"))
	if err != nil || dir == root {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		http.Error(w, "not a folder", http.StatusNotFound)
		return
	}
	if !directArchives(dir) {
		http.Error(w, "no .rar archives in this folder, so there's nothing to tell extracted files apart; delete the folder instead", http.StatusConflict)
		return
	}
	if p := serviceBusy(dir); p != "" {
		http.Error(w, "a service is still downloading or extracting in "+p+"; wait until it's done", http.StatusConflict)
		return
	}
	uploadsMu.Lock()
	defer uploadsMu.Unlock() // held so no upload lands in it meanwhile
	for _, t := range uploads {
		if strings.HasPrefix(t, dir+string(filepath.Separator)) {
			http.Error(w, "an upload is in progress there", http.StatusConflict)
			return
		}
	}

	var deleted int
	var freed int64
	failed := []string{}
	var dirs []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir {
				if strings.HasPrefix(d.Name(), ".") {
					return fs.SkipDir
				}
				dirs = append(dirs, p)
			}
			return nil
		}
		if !d.Type().IsRegular() || !extractedFile(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if err := os.Remove(p); err != nil {
			failed = append(failed, shareRel(p)+": "+err.Error())
			return nil
		}
		deleted++
		freed += info.Size()
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- { // deepest first; folders that still hold something stay
		os.Remove(dirs[i])
	}
	log.Printf("deleted %d extracted files (%d bytes) under %s", deleted, freed, dir)
	storageMu.Lock()
	storageCache = nil
	storageMu.Unlock()
	writeJSON(w, map[string]any{"deleted": deleted, "freed": freed, "failed": failed})
}

// handleDeleteArchives deletes the RAR volumes under a folder, and nothing else:
// after extracting, they're dead weight the size of the game. It refuses a folder
// a service is still downloading into, since those archives aren't extracted yet.
func handleDeleteArchives(w http.ResponseWriter, r *http.Request) {
	dir, err := resolve(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		http.Error(w, "not a folder", http.StatusNotFound)
		return
	}
	if p := serviceBusy(dir); p != "" {
		http.Error(w, "a service is still downloading or extracting in "+p+"; wait until it's done", http.StatusConflict)
		return
	}
	var deleted int
	var freed int64
	failed := []string{}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") || !d.Type().IsRegular() || !isArchive(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if err := os.Remove(p); err != nil {
			failed = append(failed, shareRel(p)+": "+err.Error())
			return nil
		}
		deleted++
		freed += info.Size()
		return nil
	})
	log.Printf("deleted %d archives (%d bytes) under %s", deleted, freed, dir)
	storageMu.Lock()
	storageCache = nil
	storageMu.Unlock()
	writeJSON(w, map[string]any{"deleted": deleted, "freed": freed, "failed": failed})
}
