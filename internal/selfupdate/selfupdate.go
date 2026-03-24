// Package selfupdate provides self-update capability for ghost binaries
// by downloading releases from GitHub.
package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
)

const repoAPI = "https://api.github.com/repos/wcatz/ghost/releases/latest"

// Release represents the subset of GitHub release API we need.
type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

// Asset represents a release asset.
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// LatestRelease fetches the latest release metadata from GitHub.
func LatestRelease() (*Release, error) {
	req, err := http.NewRequest("GET", repoAPI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch latest release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github API returned %d", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	return &rel, nil
}

// AssetName returns the expected archive name for the current platform.
func AssetName(version string) string {
	os := runtime.GOOS
	arch := runtime.GOARCH
	ext := "tar.gz"
	if os == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("ghost_%s_%s_%s.%s", version, os, arch, ext)
}

// FindAsset finds the matching asset for the current platform.
func FindAsset(rel *Release) (*Asset, error) {
	ver := strings.TrimPrefix(rel.TagName, "v")
	want := AssetName(ver)
	for i := range rel.Assets {
		if rel.Assets[i].Name == want {
			return &rel.Assets[i], nil
		}
	}
	return nil, fmt.Errorf("no release asset for %s/%s (expected %s)", runtime.GOOS, runtime.GOARCH, want)
}

// Download fetches the asset and returns the reader. Caller must close.
func Download(url string) (io.ReadCloser, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("download returned %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// ExtractBinary extracts the "ghost" binary from a .tar.gz archive.
func ExtractBinary(r io.Reader) ([]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}
		// The binary is at the root of the archive, named "ghost" or "ghost.exe".
		name := hdr.Name
		if name == "ghost" || name == "ghost.exe" {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("ghost binary not found in archive")
}

// Replace atomically replaces the binary at targetPath.
func Replace(targetPath string, newBinary []byte) error {
	// Resolve symlinks so we replace the actual file.
	resolved, err := resolveSymlinks(targetPath)
	if err != nil {
		return err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("stat %s: %w", resolved, err)
	}

	// Write to temp file next to target, then rename.
	dir := dirOf(resolved)
	tmp, err := os.CreateTemp(dir, ".ghost-update-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(newBinary); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmpPath, info.Mode()); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := os.Rename(tmpPath, resolved); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

func resolveSymlinks(path string) (string, error) {
	resolved, err := os.Readlink(path)
	if err != nil {
		// Not a symlink.
		return path, nil
	}
	return resolved, nil
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[:i]
		}
	}
	return "."
}
