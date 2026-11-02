package subs

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
)

// One release, several platforms, two archive kinds.
//
// Until this file existed the downloader only knew how to fetch
// sing-box-<ver>-windows-amd64.zip and unpack an exe out of it. On macOS that
// is not a cosmetic miss: the asset name does not exist, so the automatic
// install failed, and a hand-placed binary was never looked for either because
// BinaryPath only ever probed "<home>/bin/sing-box.exe" (issue #127). The
// platform decision is now a single table read at three points: the asset
// suffix, the executable name inside the archive, and the path the sidecar is
// started from.

// singBoxPlatform is the pair this build asks for.
func singBoxPlatform() (string, string) { return runtime.GOOS, runtime.GOARCH }

// singBoxAssetSuffix is the tail of the release asset name for one platform,
// e.g. "-windows-amd64.zip" or "-darwin-arm64.tar.gz". SagerNet zips Windows
// and tarballs everything else.
func singBoxAssetSuffix(goos, goarch string) string {
	if goos == "windows" {
		return fmt.Sprintf("-%s-%s.zip", goos, goarch)
	}
	return fmt.Sprintf("-%s-%s.tar.gz", goos, goarch)
}

// singBoxExeName is the file name of the executable inside the archive: only
// Windows appends .exe.
func singBoxExeName(goos string) string {
	if goos == "windows" {
		return "sing-box.exe"
	}
	return "sing-box"
}

// extractSingBoxBinary pulls the executable out of the release archive into
// dest. The archive kind is read off the asset name, so callers never have to
// say which one they downloaded. Both formats land on a temporary sibling and
// are renamed into place: a half-copied binary at the final path would be
// started by the next health pass and reported as a broken install.
func extractSingBoxBinary(archive []byte, assetName, exeName, dest string) error {
	lower := strings.ToLower(assetName)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return extractFromZip(archive, exeName, dest)
	case strings.HasSuffix(lower, ".tar.gz"):
		return extractFromTarGz(archive, exeName, dest)
	default:
		return fmt.Errorf("不认识的 sing-box 资产格式: %s", assetName)
	}
}

func extractFromZip(archive []byte, exeName, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return fmt.Errorf("解压 sing-box 失败: %w", err)
	}
	for _, f := range zr.File {
		if !isExeEntry(f.Name, exeName) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		return writeExecutable(dest, rc)
	}
	return fmt.Errorf("压缩包里没有 %s", exeName)
}

func extractFromTarGz(archive []byte, exeName, dest string) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("解压 sing-box 失败: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("解压 sing-box 失败: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || !isExeEntry(hdr.Name, exeName) {
			continue
		}
		// The tar reader is only valid until the next Next(), so the copy has
		// to happen here rather than after the loop breaks.
		return writeExecutable(dest, tr)
	}
	return fmt.Errorf("压缩包里没有 %s", exeName)
}

// isExeEntry matches the executable by base name, so an archive that nests it
// under sing-box-<ver>-<os>-<arch>/ reads the same as one that does not.
func isExeEntry(name, exeName string) bool {
	base := name
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		base = name[i+1:]
	}
	return strings.EqualFold(base, exeName)
}

func writeExecutable(dest string, src io.Reader) error {
	tmp := dest + ".tmp"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(tmp)
		return err
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}
