package release

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Sums maps a release file's basename to its expected lowercase-hex
// SHA-256 digest — the parsed form of SHA256SUMS.
type Sums map[string]string

// ParseSums reads a sha256sum/shasum-format file: one "<64-hex>  <name>"
// line per artifact (two spaces, or " *" before a binary-mode name).
// Malformed lines and duplicate names are errors — a sums file that
// can't be fully parsed must never be partially trusted.
func ParseSums(data []byte) (Sums, error) {
	s := Sums{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		if len(line) < 66 || !sha256Re.MatchString(line[:64]) {
			return nil, fmt.Errorf("%s: line %d: expected \"<sha256>  <file>\", got %q",
				SumsFile, lineNo, line)
		}
		rest := line[64:]
		var name string
		switch {
		case strings.HasPrefix(rest, "  "):
			name = strings.TrimLeft(rest, " ")
		case strings.HasPrefix(rest, " *"):
			name = strings.TrimPrefix(rest, " *")
		default:
			return nil, fmt.Errorf("%s: line %d: expected \"<sha256>  <file>\", got %q",
				SumsFile, lineNo, line)
		}
		if name == "" || strings.ContainsAny(name, "/\\") || name == ".." {
			return nil, fmt.Errorf("%s: line %d: bad file name %q", SumsFile, lineNo, name)
		}
		sum := strings.ToLower(line[:64])
		if prev, dup := s[name]; dup {
			return nil, fmt.Errorf("%s: line %d: duplicate entry %q (%s vs %s)",
				SumsFile, lineNo, name, prev, sum)
		}
		s[name] = sum
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(s) == 0 {
		return nil, fmt.Errorf("%s: no entries", SumsFile)
	}
	return s, nil
}

// Format emits the canonical sha256sum text form (entries sorted by
// file name for reproducibility).
func (s Sums) Format() string {
	names := make([]string, 0, len(s))
	for n := range s {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%s  %s\n", s[n], n)
	}
	return b.String()
}

// SumFile streams a file through SHA-256 and returns the lowercase-hex
// digest.
func SumFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// SumBytes digests an in-memory blob.
func SumBytes(b []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// VerifyFile checks one file against its expected digest.
func VerifyFile(path, wantHex string) error {
	got, err := SumFile(path)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(wantHex)) != 1 {
		return fmt.Errorf("%s: sha256 mismatch (want %s, got %s)",
			filepath.Base(path), wantHex, got)
	}
	return nil
}

// VerifySums checks every entry in s against files inside dir. It
// returns the sorted list of verified names; any missing file or digest
// mismatch is a hard error listing all failures, not just the first.
// Files present in dir but absent from the sums file are reported
// separately — extras never fail verification (the release directory
// legitimately contains SHA256SUMS, the manifest, and signatures) but
// callers can surface them as warnings.
func VerifySums(dir string, s Sums) (verified []string, extras []string, err error) {
	var failures []string
	names := make([]string, 0, len(s))
	for n := range s {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if verr := VerifyFile(filepath.Join(dir, n), s[n]); verr != nil {
			failures = append(failures, verr.Error())
		} else {
			verified = append(verified, n)
		}
	}
	entries, rerr := os.ReadDir(dir)
	if rerr == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if _, ok := s[e.Name()]; !ok {
				extras = append(extras, e.Name())
			}
		}
		sort.Strings(extras)
	}
	if len(failures) > 0 {
		return nil, extras, fmt.Errorf("checksum verification failed:\n  %s",
			strings.Join(failures, "\n  "))
	}
	return verified, extras, nil
}
