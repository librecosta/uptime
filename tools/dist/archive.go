package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

type member struct {
	name string
	data []byte
	mode os.FileMode
}

func writeArchive(path string, t target, files []member) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if t.os == "windows" {
		w := zip.NewWriter(f)
		defer func() { err = errors.Join(err, w.Close()) }()
		for _, file := range files {
			h := &zip.FileHeader{Name: file.name, Method: zip.Deflate}
			h.SetMode(file.mode)
			entry, err := w.CreateHeader(h)
			if err != nil {
				return err
			}
			if _, err := entry.Write(file.data); err != nil {
				return err
			}
		}
		return nil
	}
	gz := gzip.NewWriter(f)
	defer func() { err = errors.Join(err, gz.Close()) }()
	w := tar.NewWriter(gz)
	defer func() { err = errors.Join(err, w.Close()) }()
	for _, file := range files {
		if err := w.WriteHeader(&tar.Header{Name: file.name, Mode: int64(file.mode), Size: int64(len(file.data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := w.Write(file.data); err != nil {
			return err
		}
	}
	return nil
}

// readArchive validates the flat allowlist before any member can be extracted.
func readArchive(path string, t target) ([]member, error) {
	want := []string{t.binary(), "uptime.example.yaml", "README.md", "LICENSE"}
	files := make([]member, 0, len(want))
	add := func(name string, mode os.FileMode, r io.Reader) error {
		if len(files) >= len(want) || name != want[len(files)] || !mode.IsRegular() {
			return fmt.Errorf("unexpected archive member %q", name)
		}
		perm := os.FileMode(0644)
		if len(files) == 0 {
			perm = 0755
		}
		if mode.Perm() != perm {
			return fmt.Errorf("invalid archive permissions for %s", name)
		}
		const maxMember = 128 << 20
		data, err := io.ReadAll(io.LimitReader(r, maxMember+1))
		if err != nil {
			return err
		}
		if len(data) > maxMember {
			return fmt.Errorf("archive member too large: %s", name)
		}
		files = append(files, member{name, data, mode})
		return nil
	}
	if t.os == "windows" {
		r, err := zip.OpenReader(path)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		for _, entry := range r.File {
			body, err := entry.Open()
			if err != nil {
				return nil, err
			}
			err = errors.Join(add(entry.Name, entry.Mode(), body), body.Close())
			if err != nil {
				return nil, err
			}
		}
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r := tar.NewReader(gz)
		for {
			h, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			if h.Typeflag != tar.TypeReg {
				return nil, errors.New("archive member is not a regular file")
			}
			if err := add(h.Name, h.FileInfo().Mode(), r); err != nil {
				return nil, err
			}
		}
		// Read through the gzip trailer so CRC/truncation errors are checked too.
		if _, err := io.Copy(io.Discard, gz); err != nil {
			return nil, err
		}
	}
	if len(files) != len(want) {
		return nil, errors.New("archive must contain exactly four files")
	}
	return files, nil
}

func checksumManifest(dir, version string) ([]byte, error) {
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.archive(version))
	}
	sort.Strings(names)
	var manifest bytes.Buffer
	for _, name := range names {
		data, err := readRegular(dir, name)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&manifest, "%x  %s\n", sha256.Sum256(data), name)
	}
	return manifest.Bytes(), nil
}

func writeChecksums(dir, version string) error {
	manifest, err := checksumManifest(dir, version)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "SHA256SUMS"), manifest, 0644)
}

func verifyChecksums(dir, version string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) != len(targets)+1 {
		return errors.New("distribution must contain exactly five archives and SHA256SUMS")
	}
	want, err := checksumManifest(dir, version)
	if err != nil {
		return err
	}
	got, err := readRegular(dir, "SHA256SUMS")
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return errors.New("SHA256SUMS does not match the five archives in filename order")
	}
	return nil
}
