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
	"sort"
	"strings"
)

type Entry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	// ModTime is the file's modification time (Unix nanoseconds) when it
	// was hashed. While size and time are unchanged, Verify does not hash a
	// multi-gigabyte file again on every start.
	ModTime int64 `json:"mod_time,omitempty"`
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
	return Entry{Path: p, SHA256: hex.EncodeToString(h.Sum(nil)), SizeBytes: n + 4, ModTime: st.ModTime().UnixNano()}, nil
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
	if e.ModTime != 0 {
		if st, err := os.Stat(e.Path); err == nil && st.Mode().IsRegular() && st.Size() == e.SizeBytes && st.ModTime().UnixNano() == e.ModTime {
			return nil
		}
	}
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

// Found is a GGUF model file found by Scan.
type Found struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	// Parts is the number of files of a split model (Path is the first).
	Parts int `json:"parts"`
	// Suggested is an alias derived from the file name.
	Suggested string `json:"suggested"`
}

var splitPart = regexp.MustCompile(`^(.*)-(\d{5})-of-(\d{5})\.gguf$`)

// Scan lists .gguf files under dir, at most four folders deep. A model
// split into parts (name-00001-of-00003.gguf) is listed once, by its first
// part, with the size of all parts. Symbolic links to folders are not
// followed. It stops after looking at 20000 entries.
func Scan(dir string) ([]Found, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, errors.New(root + " is not a folder")
	}
	type split struct {
		first string
		size  int64
		parts int
	}
	splits := map[string]*split{}
	var found []Found
	seen := 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil // unreadable folder: skip it
		}
		seen++
		if seen > 20000 {
			return filepath.SkipAll
		}
		if d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			if rel != "." && (strings.Count(rel, string(filepath.Separator)) >= 4 || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".gguf") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		size := info.Size()
		if !info.Mode().IsRegular() {
			// A symlink, or on Windows a reparse point such as a OneDrive
			// or deduplicated file, which Go may report as irregular: use
			// it when it resolves to a regular file.
			target, err := os.Stat(path)
			if err != nil || !target.Mode().IsRegular() {
				return nil
			}
			size = target.Size()
		}
		if m := splitPart.FindStringSubmatch(filepath.Base(path)); m != nil {
			key := filepath.Join(filepath.Dir(path), m[1]) + "/" + m[3]
			sp := splits[key]
			if sp == nil {
				sp = &split{}
				splits[key] = sp
			}
			sp.size += size
			sp.parts++
			if m[2] == "00001" {
				sp.first = path
			}
			return nil
		}
		found = append(found, Found{Path: path, SizeBytes: size, Parts: 1, Suggested: Suggest(path)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, sp := range splits {
		if sp.first != "" {
			found = append(found, Found{Path: sp.first, SizeBytes: sp.size, Parts: sp.parts, Suggested: Suggest(sp.first)})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Path < found[j].Path })
	return found, nil
}

var notAlias = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// Suggest derives a valid model alias from a file name.
func Suggest(path string) string {
	name := filepath.Base(path)
	if m := splitPart.FindStringSubmatch(name); m != nil {
		name = m[1]
	}
	name = strings.TrimSuffix(strings.TrimSuffix(name, ".gguf"), ".GGUF")
	name = strings.Trim(notAlias.ReplaceAllString(strings.ToLower(name), "-"), "-._")
	if len(name) > 64 {
		name = name[:64]
	}
	if !validName.MatchString(name) {
		return "model"
	}
	return name
}

// ValidName reports whether name can be a model alias.
func ValidName(name string) bool { return validName.MatchString(name) }
