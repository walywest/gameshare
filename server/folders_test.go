package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func zipReq(p string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handleZip(w, httptest.NewRequest(http.MethodGet, "/zip?path="+url.QueryEscape(p), nil))
	return w
}

func archivesReq(p string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handleDeleteArchives(w, httptest.NewRequest(http.MethodPost, "/api/archives/delete?path="+url.QueryEscape(p), nil))
	return w
}

func TestZip(t *testing.T) {
	root = t.TempDir()
	g := filepath.Join(root, "My Game")
	os.MkdirAll(filepath.Join(g, "sub", "empty"), 0o755)
	os.MkdirAll(filepath.Join(g, ".state"), 0o755)
	os.WriteFile(filepath.Join(g, "setup.exe"), []byte("installer"), 0o644)
	os.WriteFile(filepath.Join(g, "sub", "data.bin"), bytes.Repeat([]byte("ab"), 1<<20), 0o644)
	os.WriteFile(filepath.Join(g, ".fitfast.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(g, ".state", "x"), []byte("x"), 0o644)
	os.Symlink("/etc/passwd", filepath.Join(g, "link"))

	w := zipReq("My Game")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/zip" ||
		!strings.Contains(w.Header().Get("Content-Disposition"), `My Game.zip`) {
		t.Fatalf("zip: %d %v", w.Code, w.Header())
	}
	body := w.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
		if f.Method != zip.Store {
			t.Errorf("%s: method %d, want stored", f.Name, f.Method)
		}
		if f.Name == "My Game/sub/data.bin" {
			rc, _ := f.Open()
			got, err := io.ReadAll(rc)
			if err != nil || !bytes.Equal(got, bytes.Repeat([]byte("ab"), 1<<20)) {
				t.Errorf("data.bin content wrong (%v)", err)
			}
		}
	}
	sort.Strings(names)
	want := []string{"My Game/", "My Game/setup.exe", "My Game/sub/", "My Game/sub/data.bin", "My Game/sub/empty/"}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Errorf("entries %q, want %q", names, want)
	}
	if n := len(listTransfers(t)); n != 0 {
		t.Errorf("%d transfers left after the zip", n)
	}

	hw := httptest.NewRecorder()
	handleZip(hw, httptest.NewRequest(http.MethodHead, "/zip?path=My+Game", nil))
	if hw.Code != http.StatusOK || hw.Body.Len() != 0 {
		t.Errorf("HEAD: %d with %d body bytes", hw.Code, hw.Body.Len())
	}
	if c := zipReq("My Game/setup.exe").Code; c != http.StatusNotFound {
		t.Errorf("file: got %d, want 404", c)
	}
	if c := zipReq("My Game/.state").Code; c != http.StatusBadRequest {
		t.Errorf("dot folder: got %d, want 400", c)
	}
}

func TestDeleteArchives(t *testing.T) {
	root = t.TempDir()
	partDir = filepath.Join(root, ".uploads")
	g := filepath.Join(root, "FitFast", "Game [v1.2]")
	os.MkdirAll(filepath.Join(g, "sub"), 0o755)
	os.MkdirAll(filepath.Join(g, ".hidden"), 0o755)
	write := func(rel string, n int) { os.WriteFile(filepath.Join(g, rel), make([]byte, n), 0o644) }
	gone := []string{"Game.part001.rar", "Game.part002.RAR", "old.r00", "old.r01", "sub/single.rar"}
	kept := []string{"setup.exe", "fg-01.bin", "notes.rar.txt", "rar", "x.r1", ".hidden/keep.rar", ".dot.rar"}
	for _, f := range gone {
		write(f, 100)
	}
	for _, f := range kept {
		write(f, 7)
	}

	storageCache = nil
	sw := httptest.NewRecorder()
	handleStorage(sw, httptest.NewRequest(http.MethodGet, "/api/storage", nil))
	var v storageView
	json.Unmarshal(sw.Body.Bytes(), &v)
	if len(v.Archives) != 2 || v.Archives[0].Name != "FitFast/Game [v1.2]" || v.Archives[0].Files != 4 || v.Archives[0].Size != 400 ||
		v.Archives[1].Name != "FitFast/Game [v1.2]/sub" || v.Archives[1].Size != 100 {
		t.Errorf("storage archives %+v", v.Archives)
	}

	w := archivesReq("FitFast/Game [v1.2]")
	var out struct {
		Deleted int
		Freed   int64
		Failed  []string
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != http.StatusOK || out.Deleted != 5 || out.Freed != 500 || len(out.Failed) != 0 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	for _, f := range gone {
		if _, err := os.Stat(filepath.Join(g, f)); !os.IsNotExist(err) {
			t.Errorf("%s still there", f)
		}
	}
	for _, f := range kept {
		if _, err := os.Stat(filepath.Join(g, f)); err != nil {
			t.Errorf("%s was deleted", f)
		}
	}

	if c := archivesReq("FitFast/Game [v1.2]/setup.exe").Code; c != http.StatusNotFound {
		t.Errorf("file: got %d, want 404", c)
	}
	if c := archivesReq("../x").Code; c != http.StatusNotFound && c != http.StatusBadRequest {
		t.Errorf("outside: got %d", c)
	}
	if c := archivesReq(".uploads").Code; c != http.StatusBadRequest {
		t.Errorf(".uploads: got %d, want 400", c)
	}
}

// A finished game: the download (volumes, an aria2 resume file, FitFast's state)
// beside what was extracted from it.
func gameFolder(t *testing.T) string {
	t.Helper()
	root = t.TempDir()
	partDir = filepath.Join(root, ".uploads")
	os.MkdirAll(partDir, 0o755)
	g := filepath.Join(root, "FitFast", "Game")
	os.MkdirAll(filepath.Join(g, "MD5"), 0o755)
	os.MkdirAll(filepath.Join(g, "Redist", "deep"), 0o755)
	for name, n := range map[string]int{
		"Game.part001.rar": 100, "Game.part002.rar": 100, "Game.part002.rar.aria2": 3, ".fitfast.json": 2,
		"setup.exe": 10, "fg-01.bin": 50, "MD5/fitgirl-bins.md5": 4, "Redist/deep/vc.exe": 6,
	} {
		os.WriteFile(filepath.Join(g, name), make([]byte, n), 0o644)
	}
	return g
}

func zipNames(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	body := w.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("%d %s: %v", w.Code, body, err)
	}
	var names []string
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, "/") {
			names = append(names, f.Name)
		}
	}
	sort.Strings(names)
	return names
}

func TestExtracted(t *testing.T) {
	g := gameFolder(t)

	storageCache = nil
	sw := httptest.NewRecorder()
	handleStorage(sw, httptest.NewRequest(http.MethodGet, "/api/storage", nil))
	var v storageView
	json.Unmarshal(sw.Body.Bytes(), &v)
	if len(v.Archives) != 1 || v.Archives[0].Files != 2 || v.Archives[0].Size != 200 ||
		v.Archives[0].ExtractedFiles != 4 || v.Archives[0].Extracted != 70 {
		t.Errorf("storage archives %+v", v.Archives)
	}

	w := httptest.NewRecorder()
	handleZip(w, httptest.NewRequest(http.MethodGet, "/zip?path=FitFast/Game&skip=archives", nil))
	want := "Game/MD5/fitgirl-bins.md5|Game/Redist/deep/vc.exe|Game/fg-01.bin|Game/setup.exe"
	if got := strings.Join(zipNames(t, w), "|"); got != want {
		t.Errorf("extracted zip has %s, want %s", got, want)
	}
	if got := len(zipNames(t, zipReq("FitFast/Game"))); got != 7 {
		t.Errorf("full zip has %d files, want 7", got)
	}

	for _, p := range []string{"", "FitFast", "FitFast/Game/MD5", "FitFast/Game/setup.exe", ".uploads"} {
		if c := extractedReq(p).Code; c == http.StatusOK {
			t.Errorf("%q: allowed, should be refused", p)
		}
	}
	r := extractedReq("FitFast/Game")
	var out struct {
		Deleted int
		Freed   int64
	}
	json.Unmarshal(r.Body.Bytes(), &out)
	if r.Code != http.StatusOK || out.Deleted != 4 || out.Freed != 70 {
		t.Fatalf("delete extracted: %d %s", r.Code, r.Body)
	}
	var left []string
	filepath.WalkDir(g, func(p string, d fs.DirEntry, _ error) error {
		if p != g {
			rel, _ := filepath.Rel(g, p)
			left = append(left, rel)
		}
		return nil
	})
	sort.Strings(left)
	if got := strings.Join(left, "|"); got != ".fitfast.json|Game.part001.rar|Game.part002.rar|Game.part002.rar.aria2" {
		t.Errorf("left behind: %s", got)
	}
}

func extractedReq(p string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handleDeleteExtracted(w, httptest.NewRequest(http.MethodPost, "/api/extracted/delete?path="+url.QueryEscape(p), nil))
	return w
}
