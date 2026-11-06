package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

type Entry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func inspect(path string) (Entry, error) {
	p, err := filepath.Abs(path)
	if err != nil {
		return Entry{}, err
	}
	p, err = filepath.EvalSymlinks(p)
	if err != nil {
		return Entry{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		return Entry{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Entry{}, err
	}
	if !st.Mode().IsRegular() {
		return Entry{}, errors.New("model must be a regular file")
	}
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return Entry{}, err
	}
	if string(magic[:]) != "GGUF" {
		return Entry{}, errors.New("invalid GGUF magic")
	}
	h := sha256.New()
	h.Write(magic[:])
	n, err := io.Copy(h, f)
	if err != nil {
		return Entry{}, err
	}
	return Entry{Path: p, SHA256: hex.EncodeToString(h.Sum(nil)), SizeBytes: n + 4}, nil
}
func Add(dir, name, path string) (Entry, error) {
	if !validName.MatchString(name) {
		return Entry{}, errors.New("invalid model alias")
	}
	e, err := inspect(path)
	if err != nil {
		return Entry{}, err
	}
	e.Name = name
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Entry{}, err
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return Entry{}, err
	}
	f, err := os.OpenFile(filepath.Join(dir, name+".json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Entry{}, err
	}
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return Entry{}, err
	}
	if closeErr != nil {
		return Entry{}, closeErr
	}
	return e, nil
}
func Get(dir, name string) (Entry, error) {
	if !validName.MatchString(name) {
		return Entry{}, errors.New("invalid model alias")
	}
	f, err := os.Open(filepath.Join(dir, name+".json"))
	if err != nil {
		return Entry{}, err
	}
	defer f.Close()
	var e Entry
	if err := json.NewDecoder(io.LimitReader(f, 16384)).Decode(&e); err != nil {
		return Entry{}, err
	}
	if e.Name != name || !filepath.IsAbs(e.Path) || len(e.SHA256) != 64 || e.SizeBytes < 4 {
		return Entry{}, errors.New("invalid registry entry")
	}
	if _, err := hex.DecodeString(e.SHA256); err != nil {
		return Entry{}, err
	}
	return e, nil
}
func List(dir string) ([]Entry, error) {
	fs, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []Entry{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := []Entry{}
	for _, f := range fs {
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
			continue
		}
		e, err := Get(dir, f.Name()[:len(f.Name())-5])
		if err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, nil
}
func Verify(e Entry) error {
	actual, err := inspect(e.Path)
	if err != nil {
		return err
	}
	if actual.SHA256 != e.SHA256 || actual.SizeBytes != e.SizeBytes {
		return errors.New("registered model has changed")
	}
	return nil
}

// KVInputs requires verified metadata and runtime cache formats, not guesses.
// KVBytes excludes weights, scratch buffers, padding and backend overhead.
type KVInputs struct {
	Layers, KVHeads, KeyHeadDimension, ValueHeadDimension            uint64
	KeyBytesPerElement, ValueBytesPerElement, Context, ParallelSlots uint64
}

func (k KVInputs) KVBytes() (uint64, error) {
	mul := func(a, b uint64) (uint64, error) {
		if a == 0 || b == 0 || a > ^uint64(0)/b {
			return 0, errors.New("missing or overflowing KV inputs")
		}
		return a * b, nil
	}
	a, err := mul(k.KeyHeadDimension, k.KeyBytesPerElement)
	if err != nil {
		return 0, err
	}
	b, err := mul(k.ValueHeadDimension, k.ValueBytesPerElement)
	if err != nil {
		return 0, err
	}
	if a > ^uint64(0)-b {
		return 0, errors.New("KV inputs overflow")
	}
	n := a + b
	for _, v := range []uint64{k.Layers, k.KVHeads, k.Context, k.ParallelSlots} {
		n, err = mul(n, v)
		if err != nil {
			return 0, err
		}
	}
	return n, nil
}
