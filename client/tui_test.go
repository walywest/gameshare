package main

import (
	"reflect"
	"testing"
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
