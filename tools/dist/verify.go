package main

import (
	"bytes"
	"debug/buildinfo"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"errors"
	"fmt"
	"path/filepath"
)

func verifyDistribution(dir string, opts options) error {
	if err := verifyChecksums(dir, opts.version); err != nil {
		return err
	}
	for _, t := range targets {
		name := t.archive(opts.version)
		files, err := readArchive(filepath.Join(dir, name), t)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := verifyBinary(files[0].data, t); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// verifyBinary inspects Go metadata and native executable headers without running
// foreign binaries. Version/commit injection still needs a native smoke check:
// trimmed Go builds do not retain linker flags in their build metadata.
func verifyBinary(data []byte, t target) error {
	r := bytes.NewReader(data)
	info, err := buildinfo.Read(r)
	if err != nil {
		return err
	}
	if info.Path != "github.com/librecosta/uptime/cmd/uptime" {
		return errors.New("unexpected executable package")
	}
	settings := make(map[string]string)
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	for key, want := range map[string]string{
		"GOOS": t.os, "GOARCH": t.arch, "CGO_ENABLED": "0",
		"-trimpath": "true",
	} {
		if settings[key] != want {
			return fmt.Errorf("unexpected binary build setting %s", key)
		}
	}
	switch t.os {
	case "linux":
		f, err := elf.NewFile(r)
		if err != nil {
			return err
		}
		want := elf.EM_X86_64
		if t.arch == "arm64" {
			want = elf.EM_AARCH64
		}
		if f.Machine != want || f.Class != elf.ELFCLASS64 {
			return errors.New("unexpected ELF architecture")
		}
	case "darwin":
		f, err := macho.NewFile(r)
		if err != nil {
			return err
		}
		want := macho.CpuAmd64
		if t.arch == "arm64" {
			want = macho.CpuArm64
		}
		if f.Cpu != want || f.Magic != macho.Magic64 {
			return errors.New("unexpected Mach-O architecture")
		}
	case "windows":
		f, err := pe.NewFile(r)
		if err != nil {
			return err
		}
		if f.Machine != pe.IMAGE_FILE_MACHINE_AMD64 {
			return errors.New("unexpected PE architecture")
		}
	default:
		return errors.New("unsupported binary format")
	}
	return nil
}
