package creds

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingIsEmpty(t *testing.T) {
	t.Setenv("SG_CONFIG_DIR", t.TempDir())
	f, err := Load()
	if err != nil || f.Default != "" || len(f.Servers) != 0 || f.Servers == nil {
		t.Fatalf("Load = %+v, %v", f, err)
	}
	if _, _, ok := f.Lookup(""); ok {
		t.Fatal("Lookup on empty file must fail")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "sg")
	t.Setenv("SG_CONFIG_DIR", dir)
	f := File{Default: "127.0.0.1:7419", Servers: map[string]Server{
		"127.0.0.1:7419":     {Token: "sgt_a", Insecure: true},
		"sg.example.com:443": {Token: "sgt_b", CAFile: "/tmp/ca.pem"},
	}}
	if err := Save(f); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "credentials"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode: %v %v", fi, err)
	}
	di, _ := os.Stat(dir)
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v", di.Mode().Perm())
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	name, s, ok := got.Lookup("")
	if !ok || name != "127.0.0.1:7419" || s.Token != "sgt_a" || !s.Insecure {
		t.Fatalf("default lookup = %q %+v %v", name, s, ok)
	}
	name, s, ok = got.Lookup("sg.example.com:443")
	if !ok || name != "sg.example.com:443" || s.CAFile != "/tmp/ca.pem" || s.Insecure {
		t.Fatalf("named lookup = %q %+v %v", name, s, ok)
	}
	if _, _, ok := got.Lookup("other:1"); ok {
		t.Fatal("unknown server must not be found")
	}
}
