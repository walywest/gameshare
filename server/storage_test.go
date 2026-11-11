package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestStorage(t *testing.T) {
	root = t.TempDir()
	partDir = filepath.Join(root, ".uploads")
	storageCache = nil
	os.MkdirAll(filepath.Join(root, "Game", "sub", ".hidden"), 0o755)
	os.MkdirAll(partDir, 0o755)
	os.WriteFile(filepath.Join(root, "Game", "a.bin"), make([]byte, 300), 0o644)
	os.WriteFile(filepath.Join(root, "Game", "sub", "b.bin"), make([]byte, 200), 0o644)
	os.WriteFile(filepath.Join(root, "Game", "sub", ".hidden", "c.bin"), make([]byte, 999), 0o644)
	os.WriteFile(filepath.Join(root, "small.txt"), make([]byte, 10), 0o644)
	os.WriteFile(filepath.Join(partDir, "x.part"), make([]byte, 50), 0o644)

	w := httptest.NewRecorder()
	handleStorage(w, httptest.NewRequest(http.MethodGet, "/api/storage", nil))
	var v storageView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Share != 510 || v.Partials != 50 || v.PartN != 1 {
		t.Errorf("share %d partials %d/%d", v.Share, v.Partials, v.PartN)
	}
	if len(v.Folders) != 2 || v.Folders[0].Name != "Game" || v.Folders[0].Size != 500 || v.Folders[0].Files != 2 || !v.Folders[0].Dir {
		t.Errorf("folders %+v", v.Folders)
	}
	if v.Total <= 0 || v.Free <= 0 || v.Free > v.Total {
		t.Errorf("disk total %d free %d", v.Total, v.Free)
	}
}

func listTransfers(t *testing.T) []transfer {
	w := httptest.NewRecorder()
	handleTransfers(w, httptest.NewRequest(http.MethodGet, "/api/transfers", nil))
	var out struct{ Transfers []transfer }
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Transfers
}

// A download shows up while it runs, with its resume offset and progress, and is
// gone once it ends.
func TestTransfers(t *testing.T) {
	root = t.TempDir()
	os.WriteFile(filepath.Join(root, "big.bin"), make([]byte, 8<<20), 0o644)

	srv := httptest.NewServer(http.HandlerFunc(handleDownload))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/dl?path=big.bin", nil)
	req.Header.Set("Range", "bytes=1024-")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.CopyN(io.Discard, resp.Body, 4096) // the handler is now mid-copy, blocked on us

	list := listTransfers(t)
	if len(list) != 1 {
		t.Fatalf("got %d transfers mid-download", len(list))
	}
	tr := list[0]
	if tr.Kind != "download" || tr.Path != "big.bin" || tr.Size != 8<<20 || tr.Start != 1024 || tr.Done <= 1024 || tr.Client == "" {
		t.Errorf("transfer %+v", tr)
	}

	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	srv.Close() // waits for the handler to return
	if n := len(listTransfers(t)); n != 0 {
		t.Errorf("%d transfers left after the download ended", n)
	}
}
