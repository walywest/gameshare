package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func deleteReq(p string) int {
	w := httptest.NewRecorder()
	handleDelete(w, httptest.NewRequest(http.MethodDelete, "/api/delete?path="+url.QueryEscape(p), nil))
	return w.Code
}

func TestDelete(t *testing.T) {
	root = t.TempDir()
	partDir = filepath.Join(root, ".uploads")
	os.MkdirAll(filepath.Join(root, "game", "sub"), 0o755)
	os.WriteFile(filepath.Join(root, "game", "sub", "a.bin"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "single.bin"), []byte("x"), 0o644)
	os.MkdirAll(partDir, 0o755)

	for _, p := range []string{"", "/", "..", ".uploads", "game/../.uploads"} {
		if c := deleteReq(p); c != http.StatusBadRequest {
			t.Errorf("%q: got %d, want 400", p, c)
		}
	}
	if _, err := os.Stat(partDir); err != nil {
		t.Fatal("partial-upload folder was removed")
	}
	if c := deleteReq("missing"); c != http.StatusNotFound {
		t.Errorf("missing: got %d", c)
	}

	uploads["x.part"] = filepath.Join(root, "game", "sub", "b.bin")
	if c := deleteReq("game"); c != http.StatusConflict {
		t.Errorf("busy folder: got %d", c)
	}
	delete(uploads, "x.part")

	if c := deleteReq("single.bin"); c != http.StatusOK {
		t.Errorf("file: got %d", c)
	}
	if c := deleteReq("game"); c != http.StatusOK {
		t.Errorf("folder: got %d", c)
	}
	for _, p := range []string{"single.bin", "game"} {
		if _, err := os.Stat(filepath.Join(root, p)); !os.IsNotExist(err) {
			t.Errorf("%s still there", p)
		}
	}

	w := httptest.NewRecorder()
	handleDelete(w, httptest.NewRequest(http.MethodGet, "/api/delete?path=x", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: got %d", w.Code)
	}
}
