package objfmt

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestBlobSHA(t *testing.T) {
	if got := BlobSHA([]byte("hello\n")); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("got %s", got)
	}
	if got := BlobSHA(nil); got != "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391" {
		t.Fatalf("empty: got %s", got)
	}
}

func TestParseTree(t *testing.T) {
	a, _ := hex.DecodeString("ce013625030ba8dba906f756967f9e9ca394464a")
	d, _ := hex.DecodeString("4b825dc642cb6eb9a060e54bf8d69288fbee4904")
	var raw bytes.Buffer
	raw.WriteString("100644 a b.txt\x00")
	raw.Write(a)
	raw.WriteString("40000 dir\x00")
	raw.Write(d)
	got, err := ParseTree(raw.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	want := []TreeEntry{
		{Mode: "100644", Name: "a b.txt", SHA: "ce013625030ba8dba906f756967f9e9ca394464a"},
		{Mode: "40000", Name: "dir", SHA: "4b825dc642cb6eb9a060e54bf8d69288fbee4904"},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] || got[0].IsTree() || !got[1].IsTree() {
		t.Fatalf("got %+v", got)
	}
	if _, err := ParseTree([]byte("100644 truncated\x00abc")); err == nil {
		t.Fatal("want error for truncated tree")
	}
}
