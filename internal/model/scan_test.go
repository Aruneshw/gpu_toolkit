package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeGGUF(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte("GGUF"), make([]byte, size-4)...), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestScanFindsModelsAndSplitParts(t *testing.T) {
	root := t.TempDir()
	writeGGUF(t, filepath.Join(root, "Qwen2.5-7B-Instruct-Q4_K_M.gguf"), 100)
	writeGGUF(t, filepath.Join(root, "big", "Llama-70B-00001-of-00003.gguf"), 10)
	writeGGUF(t, filepath.Join(root, "big", "Llama-70B-00002-of-00003.gguf"), 20)
	writeGGUF(t, filepath.Join(root, "big", "Llama-70B-00003-of-00003.gguf"), 30)
	writeGGUF(t, filepath.Join(root, "a", "b", "c", "d", "e", "too-deep.gguf"), 10)
	writeGGUF(t, filepath.Join(root, ".cache", "hidden.gguf"), 10)
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	// Links need a privilege on Windows, and Wine reports success without
	// creating them: test links only where they really exist.
	links := os.Symlink(filepath.Join(root, "Qwen2.5-7B-Instruct-Q4_K_M.gguf"), filepath.Join(root, "link.gguf")) == nil &&
		os.Symlink(root, filepath.Join(root, "loop")) == nil
	if links {
		_, err1 := os.Lstat(filepath.Join(root, "link.gguf"))
		_, err2 := os.Lstat(filepath.Join(root, "loop"))
		links = err1 == nil && err2 == nil
	}
	if !links {
		t.Log("symbolic links are not available here; skipping the link checks")
	}
	found, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Found{}
	for _, f := range found {
		rel, _ := filepath.Rel(root, f.Path)
		got[rel] = f
	}
	if want := map[bool]int{true: 3, false: 2}[links]; len(got) != want {
		t.Fatalf("want %d models: %+v", want, found)
	}
	q := got["Qwen2.5-7B-Instruct-Q4_K_M.gguf"]
	if q.SizeBytes != 100 || q.Parts != 1 || q.Suggested != "qwen2.5-7b-instruct-q4_k_m" {
		t.Fatalf("%+v", q)
	}
	l := got[filepath.Join("big", "Llama-70B-00001-of-00003.gguf")]
	if l.SizeBytes != 60 || l.Parts != 3 || l.Suggested != "llama-70b" {
		t.Fatalf("split model %+v", l)
	}
	if link := got["link.gguf"]; links && link.SizeBytes != 100 {
		t.Fatalf("symlinked file %+v", link)
	}
	if _, err := Scan(filepath.Join(root, "notes.txt")); err == nil {
		t.Fatal("scanned a file as a folder")
	}
	if _, err := Scan(filepath.Join(root, "missing")); err == nil {
		t.Fatal("scanned a missing folder")
	}
}

func TestSuggest(t *testing.T) {
	for in, want := range map[string]string{"/m/My Model (v2).GGUF": "my-model-v2", "/x/---.gguf": "model", "/x/" + strings.Repeat("a", 80) + ".gguf": strings.Repeat("a", 64)} {
		if got := Suggest(in); got != want || !ValidName(got) {
			t.Errorf("Suggest(%q) = %q, want %q", in, got, want)
		}
	}
}

// Verify trusts an unchanged size and modification time instead of hashing
// again, and still notices a changed file.
func TestVerifyFastPathAndChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.gguf")
	writeGGUF(t, path, 64)
	e, err := Add(filepath.Join(dir, "reg"), "m", path)
	if err != nil || e.ModTime == 0 {
		t.Fatalf("%+v %v", e, err)
	}
	if err := Verify(e); err != nil {
		t.Fatal(err)
	}
	// Same size, new content and a new time: must hash and refuse.
	if err := os.WriteFile(path, append([]byte("GGUF"), make([]byte, 59)...), 0600); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	f.Write([]byte{1})
	f.Close()
	later := time.Now().Add(time.Minute)
	os.Chtimes(path, later, later)
	if err := Verify(e); err == nil {
		t.Fatal("changed model accepted")
	}
	// An old entry without a time is always hashed.
	e2 := e
	e2.ModTime = 0
	if err := Verify(e2); err == nil {
		t.Fatal("changed model accepted without a recorded time")
	}
}
