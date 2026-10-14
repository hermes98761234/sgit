package wc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteReadLocalModes(t *testing.T) {
	root := t.TempDir()
	if err := WriteLocal(root, "bin/run.sh", []byte("#!/bin/sh\n"), "100755"); err != nil {
		t.Fatal(err)
	}
	if err := WriteLocal(root, "link", []byte("bin/run.sh"), "120000"); err != nil {
		t.Fatal(err)
	}
	if err := WriteLocal(root, "a.txt", []byte("x"), "100644"); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(root, "bin", "run.sh"))
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("exec mode = %v", fi.Mode().Perm())
	}
	data, mode, err := ReadLocal(root, "link")
	if err != nil || mode != "120000" || string(data) != "bin/run.sh" {
		t.Fatalf("symlink = %q %s %v", data, mode, err)
	}
	if _, mode, _ := ReadLocal(root, "bin/run.sh"); mode != "100755" {
		t.Fatalf("exec read mode = %s", mode)
	}
	if _, mode, _ := ReadLocal(root, "a.txt"); mode != "100644" {
		t.Fatalf("plain read mode = %s", mode)
	}
	// Overwrite a symlink with a regular file.
	if err := WriteLocal(root, "link", []byte("now a file"), "100644"); err != nil {
		t.Fatal(err)
	}
	if data, mode, _ := ReadLocal(root, "link"); mode != "100644" || string(data) != "now a file" {
		t.Fatalf("replaced link = %q %s", data, mode)
	}
}

func TestWriteLocalReplacesFileWithDirectory(t *testing.T) {
	root := t.TempDir()
	_ = WriteLocal(root, "d", []byte("file"), "100644")
	if err := WriteLocal(root, "d/inner.txt", []byte("in"), "100644"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(root, "d")); !fi.IsDir() {
		t.Fatal("d must now be a directory")
	}
	if err := WriteLocal(root, "d", []byte("file again"), "100644"); err == nil {
		t.Fatal("a non-empty directory in the way must be an error")
	}
}

func TestRemoveLocalPrunesEmptyDirs(t *testing.T) {
	root := t.TempDir()
	_ = WriteLocal(root, "x/y/z.txt", []byte("z"), "100644")
	_ = WriteLocal(root, "x/keep.txt", []byte("k"), "100644")
	if err := RemoveLocal(root, "x/y/z.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "x", "y")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty x/y must be pruned")
	}
	if _, err := os.Stat(filepath.Join(root, "x", "keep.txt")); err != nil {
		t.Fatal("non-empty x must stay")
	}
	if err := RemoveLocal(root, "missing.txt"); err != nil {
		t.Fatalf("removing a missing file is fine: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("root must never be removed")
	}
}

func TestPathConfinement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "wc")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside.txt")
	if err := os.WriteFile(outside, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", ".", "../esc", "a/../../esc", "/abs", ".sg/base", ".SG/x", "a//b", "a\\b"} {
		if err := WriteLocal(root, p, []byte("x"), "100644"); err == nil {
			t.Errorf("WriteLocal(%q) must fail", p)
		}
		if err := RemoveLocal(root, p); err == nil {
			t.Errorf("RemoveLocal(%q) must fail", p)
		}
		if _, _, err := ReadLocal(root, p); err == nil {
			t.Errorf("ReadLocal(%q) must fail", p)
		}
	}
	if err := RemoveLocal(root, "../outside.txt"); err == nil {
		t.Error("RemoveLocal(../outside.txt) must fail")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("sibling file was removed: %v", err)
	}
	if err := WriteLocal(root, "../created.txt", []byte("x"), "100644"); err == nil {
		t.Error("WriteLocal(../created.txt) must fail")
	}
	if _, err := os.Stat(filepath.Join(parent, "created.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("WriteLocal wrote outside the working copy")
	}
	if err := RemoveLocal(root, ""); err == nil {
		t.Error("RemoveLocal(\"\") must fail")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("root must never be removed")
	}
}

func TestStatEntry(t *testing.T) {
	root := t.TempDir()
	_ = WriteLocal(root, "a.txt", []byte("abc"), "100644")
	if e, err := StatEntry(root, "a.txt", "100644", "sha"); err != nil || e.Size != -1 || e.MtimeNs == 0 {
		t.Fatalf("fresh file StatEntry = %+v, %v; want Size -1", e, err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(root, "a.txt"), old, old); err != nil {
		t.Fatal(err)
	}
	e, err := StatEntry(root, "a.txt", "100644", "sha")
	if err != nil || e.Size != 3 || e.MtimeNs != old.UnixNano() || e.Path != "a.txt" || e.SHA != "sha" {
		t.Fatalf("StatEntry = %+v, %v", e, err)
	}
}
