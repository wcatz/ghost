package tui

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color/palette"
	"image/draw"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"

	"github.com/BourgeoisBear/rasterm"
)

// imageProtocol identifies the terminal image protocol to use.
type imageProtocol int

const (
	imageNone   imageProtocol = iota
	imageSixel
	imageKitty
	imageITerm2
)

// detectImageProtocol auto-detects terminal image capability.
func detectImageProtocol() imageProtocol {
	term := os.Getenv("TERM_PROGRAM")
	switch strings.ToLower(term) {
	case "iterm.app", "iterm2", "wezterm":
		return imageITerm2
	}

	if os.Getenv("KITTY_WINDOW_ID") != "" {
		return imageKitty
	}

	// Check for sixel support via TERM.
	termEnv := os.Getenv("TERM")
	if strings.Contains(termEnv, "sixel") {
		return imageSixel
	}

	// Default: check if rasterm detects sixel.
	if capable, err := rasterm.IsSixelCapable(); err == nil && capable {
		return imageSixel
	}

	return imageNone
}

// parseImageProtocol converts a config string to imageProtocol.
func parseImageProtocol(s string) imageProtocol {
	switch strings.ToLower(s) {
	case "sixel":
		return imageSixel
	case "kitty":
		return imageKitty
	case "iterm2":
		return imageITerm2
	case "none", "off", "disabled":
		return imageNone
	default:
		return detectImageProtocol()
	}
}

// renderImageFile renders an image file to a terminal string.
// Returns a placeholder string if rendering isn't supported.
func renderImageFile(path string, protocol imageProtocol) string {
	if protocol == imageNone {
		return imagePlaceholder(path)
	}

	f, err := os.Open(path)
	if err != nil {
		return imagePlaceholder(path)
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		return imagePlaceholder(path)
	}

	var buf bytes.Buffer
	switch protocol {
	case imageSixel:
		// SixelWriteImage requires *image.Paletted — quantize if needed.
		palImg, ok := img.(*image.Paletted)
		if !ok {
			bounds := img.Bounds()
			palImg = image.NewPaletted(bounds, palette.Plan9)
			draw.FloydSteinberg.Draw(palImg, bounds, img, bounds.Min)
		}
		if err := rasterm.SixelWriteImage(&buf, palImg); err != nil {
			return imagePlaceholder(path)
		}
	case imageKitty:
		if err := rasterm.KittyWriteImage(&buf, img, rasterm.KittyImgOpts{}); err != nil {
			return imagePlaceholder(path)
		}
	case imageITerm2:
		if err := rasterm.ItermWriteImage(&buf, img); err != nil {
			return imagePlaceholder(path)
		}
	default:
		return imagePlaceholder(path)
	}

	return buf.String()
}

// imagePlaceholder returns a text fallback for unsupported terminals.
func imagePlaceholder(path string) string {
	name := filepath.Base(path)
	return fmt.Sprintf("[Image: %s]", name)
}

// isImagePath checks if a file path points to a supported image format.
func isImagePath(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp":
		return true
	}
	return false
}

// loadImageBase64 reads an image file and returns its base64 encoding and media type.
func loadImageBase64(path string) (data string, mediaType string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}

	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".png":
		mediaType = "image/png"
	case ".jpg", ".jpeg":
		mediaType = "image/jpeg"
	case ".gif":
		mediaType = "image/gif"
	case ".webp":
		mediaType = "image/webp"
	default:
		mediaType = "application/octet-stream"
	}

	data = base64.StdEncoding.EncodeToString(raw)
	return data, mediaType, nil
}
