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

	"charm.land/lipgloss/v2"
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

	img, format, err := image.Decode(f)
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

	_ = format // could log this
	return buf.String()
}

// renderImageHalfBlock converts raw image bytes into colorful Unicode half-block
// characters using lipgloss styles. Works in any true-color terminal without
// needing sixel/kitty/iTerm2 protocols. Each character cell represents 2 vertical
// pixels using ▀ (upper half block) with foreground = top pixel, background = bottom.
func renderImageHalfBlock(imgData []byte, maxCols int) string {
	img, _, err := image.Decode(bytes.NewReader(imgData))
	if err != nil {
		return ""
	}

	bounds := img.Bounds()
	scale := float64(maxCols) / float64(bounds.Dx())
	newW := maxCols
	newH := int(float64(bounds.Dy()) * scale)
	if newH%2 != 0 {
		newH++
	}

	scaled := scaleImageNearest(img, newW, newH)

	var lines []string
	for y := 0; y < newH; y += 2 {
		var line strings.Builder
		for x := 0; x < newW; x++ {
			tr, tg, tb, ta := scaled.At(x, y).RGBA()
			br, bg, bb, ba := scaled.At(x, y+1).RGBA()

			tr8, tg8, tb8 := uint8(tr>>8), uint8(tg>>8), uint8(tb>>8)
			br8, bg8, bb8 := uint8(br>>8), uint8(bg>>8), uint8(bb>>8)

			topT := ta < 0x8000
			botT := ba < 0x8000

			switch {
			case topT && botT:
				line.WriteByte(' ')
			case topT:
				s := lipgloss.NewStyle().Foreground(lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", br8, bg8, bb8)))
				line.WriteString(s.Render("▄"))
			case botT:
				s := lipgloss.NewStyle().Foreground(lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", tr8, tg8, tb8)))
				line.WriteString(s.Render("▀"))
			default:
				s := lipgloss.NewStyle().
					Foreground(lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", tr8, tg8, tb8))).
					Background(lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", br8, bg8, bb8)))
				line.WriteString(s.Render("▀"))
			}
		}
		lines = append(lines, line.String())
	}

	return strings.Join(lines, "\n")
}

// scaleImageNearest scales an image using nearest-neighbor interpolation.
func scaleImageNearest(src image.Image, newW, newH int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	bounds := src.Bounds()
	for y := 0; y < newH; y++ {
		for x := 0; x < newW; x++ {
			srcX := bounds.Min.X + x*bounds.Dx()/newW
			srcY := bounds.Min.Y + y*bounds.Dy()/newH
			dst.Set(x, y, src.At(srcX, srcY))
		}
	}
	return dst
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
