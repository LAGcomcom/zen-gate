package subs

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// The release asset this build asks for has to match the platform it runs on:
// asking for windows-amd64.zip on macOS is why the automatic install never
// worked there, and the sidecar stayed invisible (issue #127).
func TestSingBoxAssetMatchesPlatform(t *testing.T) {
	cases := []struct{ goos, goarch, want string }{
		{"windows", "amd64", "-windows-amd64.zip"},
		{"windows", "arm64", "-windows-arm64.zip"},
		{"darwin", "arm64", "-darwin-arm64.tar.gz"},
		{"darwin", "amd64", "-darwin-amd64.tar.gz"},
		{"linux", "amd64", "-linux-amd64.tar.gz"},
	}
	for _, c := range cases {
		if got := singBoxAssetSuffix(c.goos, c.goarch); got != c.want {
			t.Errorf("suffix(%s/%s) = %q, want %q", c.goos, c.goarch, got, c.want)
		}
	}
	if got := singBoxExeName("windows"); got != "sing-box.exe" {
		t.Errorf("windows exe = %q", got)
	}
	if got := singBoxExeName("darwin"); got != "sing-box" {
		t.Errorf("darwin exe = %q, want the bare name", got)
	}
}

// A nested path inside the archive must still resolve: SagerNet tarballs put
// the binary under sing-box-<ver>-<os>-<arch>/.
func TestExtractSingBoxFromTarGz(t *testing.T) {
	payload := []byte("#!/bin/sh\necho sidecar\n")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tr := tar.NewWriter(gz)
	if err := tr.WriteHeader(&tar.Header{Name: "sing-box-1.14.4-darwin-arm64/sing-box", Mode: 0o755, Size: int64(len(payload)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "sing-box")
	if err := extractSingBoxBinary(buf.Bytes(), "sing-box-1.14.4-darwin-arm64.tar.gz", "sing-box", dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("extracted %q, want the payload", got)
	}
}

func TestExtractSingBoxFromZip(t *testing.T) {
	payload := []byte("MZ fake exe")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("sing-box-1.14.4-windows-amd64/sing-box.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "sing-box.exe")
	if err := extractSingBoxBinary(buf.Bytes(), "sing-box-1.14.4-windows-amd64.zip", "sing-box.exe", dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("extracted %q", got)
	}
}

// A binary missing from the archive is an install failure with a name the user
// can act on, not a silent success that leaves BinaryPath empty.
func TestExtractSingBoxMissingBinaryIsAnError(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if _, err := zw.Create("README.md"); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	err := extractSingBoxBinary(buf.Bytes(), "sing-box-1.14.4-windows-amd64.zip", "sing-box.exe", filepath.Join(t.TempDir(), "x.exe"))
	if err == nil {
		t.Fatal("a zip without the binary must fail")
	}
}
