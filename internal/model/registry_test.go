package model

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "weights.gguf")
	reg := filepath.Join(dir, "registry")
	if err := os.WriteFile(path, []byte("GGUFfixture"), 0600); err != nil {
		t.Fatal(err)
	}
	e, err := Add(reg, "tiny", path)
	if err != nil {
		t.Fatal(err)
	}
	if e.SizeBytes != 11 || len(e.SHA256) != 64 {
		t.Fatalf("bad entry %+v", e)
	}
	got, err := Get(reg, "tiny")
	if err != nil || got != e {
		t.Fatalf("get %+v %v", got, err)
	}
	if err := Verify(e); err != nil {
		t.Fatal(err)
	}
	list, err := List(reg)
	if err != nil || len(list) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
	if _, err := Add(reg, "tiny", path); err == nil {
		t.Fatal("overwrote alias")
	}
	for _, name := range []string{"../escape", "", "a/b", "a,b"} {
		if _, err := Add(reg, name, path); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	if err := os.WriteFile(path, []byte("GGUFchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(e); err == nil {
		t.Fatal("missed changed bytes")
	}
	if err := os.WriteFile(path, []byte("NOPE"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(reg, "invalid", path); err == nil {
		t.Fatal("accepted bad magic")
	}
}
func TestKV(t *testing.T) {
	k := KVInputs{Layers: 2, KVHeads: 2, KeyHeadDimension: 4, ValueHeadDimension: 4, KeyBytesPerElement: 2, ValueBytesPerElement: 2, Context: 8, ParallelSlots: 1}
	n, err := k.KVBytes()
	if err != nil || n != 512 {
		t.Fatalf("%d %v", n, err)
	}
	k.Context = 0
	if _, err := k.KVBytes(); err == nil {
		t.Fatal("accepted unknown context")
	}
	k.Context = ^uint64(0)
	if _, err := k.KVBytes(); err == nil {
		t.Fatal("accepted overflow")
	}
}
