package imagerender

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"
	"unsafe"

	"github.com/BourgeoisBear/rasterm"
	"github.com/adrg/xdg"
	"golang.org/x/image/draw"
)

var (
	chafaAvailable     bool
	chafaChecked       bool
	kittyCapable       bool
	cache              = make(map[string]CachedImage)
	cacheMu            sync.RWMutex
	maxDiskCacheImages = 500
	ttyFile            *os.File
	nextImageID        uint32 = 1

	pngCache   = make(map[string][]byte)
	pngCacheMu sync.RWMutex
)

type CachedImage struct {
	FilePath        string `json:"path"`
	PlaceholderRows int    `json:"rows"`
	RenderedArt     string `json:"art,omitempty"`
}

func CheckChafa() bool {
	if chafaChecked {
		return chafaAvailable
	}
	chafaChecked = true
	_, err := exec.LookPath("chafa")
	chafaAvailable = err == nil
	return chafaAvailable
}

func IsAvailable() bool {
	return chafaAvailable || kittyCapable
}

func CheckKitty() bool {
	kittyCapable = rasterm.IsKittyCapable()
	return kittyCapable
}

func IsKittyCapable() bool {
	return kittyCapable
}

func initTTY() {
	if ttyFile != nil {
		return
	}
	f, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err == nil {
		ttyFile = f
	}
}

// PrepareKittyImage loads an image file, resizes it for the given cell
// dimensions, encodes to PNG, and caches the bytes in memory for fast
// re-display.  Call this once per image (background goroutine).
func PrepareKittyImage(messageID, filePath string, widthCols, heightRows int) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	srcImg, _, err := image.Decode(f)
	if err != nil {
		return fmt.Errorf("decode image: %w", err)
	}

	cellW, cellH := getCellPixelSize()
	if cellW <= 0 || cellH <= 0 {
		cellW = 8
		cellH = 16
	}

	pixW := widthCols * cellW
	pixH := heightRows * cellH

	srcBounds := srcImg.Bounds()
	scaleW := float64(pixW) / float64(srcBounds.Dx())
	scaleH := float64(pixH) / float64(srcBounds.Dy())
	scale := scaleW
	if scaleH < scaleW {
		scale = scaleH
	}

	dstW := int(float64(srcBounds.Dx()) * scale)
	dstH := int(float64(srcBounds.Dy()) * scale)
	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), srcImg, srcBounds, draw.Over, nil)

	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return fmt.Errorf("encode png: %w", err)
	}

	pngCacheMu.Lock()
	pngCache[messageID] = buf.Bytes()
	pngCacheMu.Unlock()

	return nil
}

// DisplayKittyImage writes a cached PNG image to /dev/tty at the given
// terminal cell position using the Kitty graphics protocol.  This is fast
// because PNG encoding was already done in PrepareKittyImage.
func DisplayKittyImage(messageID string, col, row, widthCols, heightRows int) {
	initTTY()
	if ttyFile == nil {
		return
	}

	pngCacheMu.RLock()
	data, ok := pngCache[messageID]
	pngCacheMu.RUnlock()
	if !ok || len(data) == 0 {
		return
	}

	imgID := nextImageID
	nextImageID++

	// Move cursor to target position
	fmt.Fprintf(ttyFile, "\x1b[%d;%dH", row+1, col+1)

	opts := rasterm.KittyImgOpts{
		DstCols: uint32(widthCols),
		DstRows: uint32(heightRows),
		ImageId: imgID,
	}
	rasterm.KittyCopyPNGInline(ttyFile, bytes.NewReader(data), opts)
}

// ClearAllKittyPlacements deletes all Kitty graphics from the terminal.
func ClearAllKittyPlacements() {
	initTTY()
	if ttyFile == nil {
		return
	}
	ttyFile.Write([]byte("\x1b_Ga=d,d=A,q=2;\x1b\\"))
}

// PlaceholderLines returns a string with visible markers for tview to reserve
// space for Kitty graphics.
func PlaceholderLines(rows int) string {
	var b strings.Builder
	for i := 0; i < rows; i++ {
		b.WriteString("[-::d]~[::-]\n")
	}
	return b.String()
}

// HasPreparedPNG returns true if the message has cached PNG data ready.
func HasPreparedPNG(messageID string) bool {
	pngCacheMu.RLock()
	defer pngCacheMu.RUnlock()
	_, ok := pngCache[messageID]
	return ok
}

// ClearPNGCache removes all cached PNG data from memory.
func ClearPNGCache() {
	pngCacheMu.Lock()
	defer pngCacheMu.Unlock()
	pngCache = make(map[string][]byte)
}

func getCellPixelSize() (int, int) {
	f, err := os.Open("/dev/tty")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	return getPixelSizeTIOCGWINSZ(f)
}

func getPixelSizeTIOCGWINSZ(f *os.File) (cellW, cellH int) {
	var ws [4]uint16
	fd := f.Fd()
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		return 0, 0
	}
	rows, cols, xpix, ypix := ws[0], ws[1], ws[2], ws[3]
	if cols > 0 && rows > 0 && xpix > 0 && ypix > 0 {
		return int(xpix) / int(cols), int(ypix) / int(rows)
	}
	return 0, 0
}

// --- Fallback: chafa character-art rendering ---

func RenderImage(imagePath string, widthCols int) (string, error) {
	if !chafaAvailable {
		return "", fmt.Errorf("chafa not available")
	}

	if widthCols < 20 {
		widthCols = 20
	}
	heightRows := widthCols / 2
	if heightRows < 10 {
		heightRows = 10
	}
	if heightRows > 30 {
		heightRows = 30
	}

	sizeArg := fmt.Sprintf("%dx%d", widthCols, heightRows)
	cmd := exec.Command("chafa",
		"--format=symbols",
		"--colors=full",
		"--color-space=din99d",
		"--size="+sizeArg,
		imagePath,
	)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = nil

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("chafa failed: %w", err)
	}

	ansiOutput := stdout.Bytes()
	if len(ansiOutput) == 0 {
		return "", fmt.Errorf("chafa produced empty output")
	}

	return ansiToTviewTags(ansiOutput), nil
}

func ansiToTviewTags(data []byte) string {
	var out strings.Builder
	out.Grow(len(data))

	var curFG, curBG, curAttrs string
	s := string(data)
	i := 0
	for i < len(s) {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] >= 0x20 && s[j] <= 0x3F) {
				j++
			}
			if j < len(s) && s[j] >= 0x40 && s[j] <= 0x7E {
				if s[j] == 'm' {
					params := s[i+2 : j]
					fg, bg, attrs := parseSGRValues(params, curFG, curBG, curAttrs)
					if fg != curFG || bg != curBG || attrs != curAttrs {
						curFG, curBG, curAttrs = fg, bg, attrs
						colon := ""
						if curAttrs != "" {
							colon = ":"
						}
						fmt.Fprintf(&out, "[%s:%s%s%s]", curFG, curBG, colon, curAttrs)
					}
				}
				i = j + 1
				continue
			}
			i = j
			continue
		}
		if s[i] == '\x1b' {
			i++
			if i < len(s) {
				i++
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == '[' {
			out.WriteString("[[]")
		} else {
			out.WriteRune(r)
		}
		i += size
	}
	return out.String()
}

func parseSGRValues(params, prevFG, prevBG, prevAttrs string) (fg, bg, attrs string) {
	fg, bg, attrs = prevFG, prevBG, prevAttrs

	if params == "" || params == "0" {
		return "-", "-", "-"
	}

	fields := strings.Split(params, ";")
	idx := 0
	for idx < len(fields) {
		f := fields[idx]
		switch f {
		case "1", "01":
			if !strings.ContainsRune(attrs, 'b') {
				attrs += "b"
			}
		case "2", "02":
			if !strings.ContainsRune(attrs, 'd') {
				attrs += "d"
			}
		case "4", "04":
			if !strings.ContainsRune(attrs, 'u') {
				attrs += "u"
			}
		case "22":
			attrs = strings.ReplaceAll(attrs, "b", "")
			attrs = strings.ReplaceAll(attrs, "d", "")
		case "24":
			attrs = strings.ReplaceAll(attrs, "u", "")
		case "30", "31", "32", "33", "34", "35", "36", "37":
			n, _ := strconv.Atoi(f)
			fg = basicColor(n - 30)
		case "39":
			fg = "-"
		case "40", "41", "42", "43", "44", "45", "46", "47":
			n, _ := strconv.Atoi(f)
			bg = basicColor(n - 40)
		case "49":
			bg = "-"
		case "90", "91", "92", "93", "94", "95", "96", "97":
			n, _ := strconv.Atoi(f)
			fg = basicColor(n - 82)
		case "100", "101", "102", "103", "104", "105", "106", "107":
			n, _ := strconv.Atoi(f)
			bg = basicColor(n - 92)
		case "38":
			c, advance := parseExtColor(fields, idx)
			if c != "" {
				fg = c
			}
			idx += advance
		case "48":
			c, advance := parseExtColor(fields, idx)
			if c != "" {
				bg = c
			}
			idx += advance
		}
		idx++
	}
	return fg, bg, attrs
}

func parseExtColor(fields []string, idx int) (string, int) {
	if idx+1 >= len(fields) {
		return "", 0
	}
	switch fields[idx+1] {
	case "5":
		if idx+2 < len(fields) {
			n, _ := strconv.Atoi(fields[idx+2])
			return color256(n), 2
		}
	case "2":
		if idx+4 < len(fields) {
			r, _ := strconv.Atoi(fields[idx+2])
			g, _ := strconv.Atoi(fields[idx+3])
			b, _ := strconv.Atoi(fields[idx+4])
			return fmt.Sprintf("#%02x%02x%02x", r, g, b), 4
		}
	}
	return "", 0
}

func basicColor(n int) string {
	colors := []string{
		"black", "maroon", "green", "olive",
		"navy", "purple", "teal", "silver",
		"gray", "red", "lime", "yellow",
		"blue", "fuchsia", "aqua", "white",
	}
	if n < 0 || n >= len(colors) {
		return "white"
	}
	return colors[n]
}

func color256(n int) string {
	if n <= 15 {
		return basicColor(n)
	}
	if n <= 231 {
		r := (n - 16) / 36
		g := ((n - 16) / 6) % 6
		b := (n - 16) % 6
		return fmt.Sprintf("#%02x%02x%02x", 255*r/5, 255*g/5, 255*b/5)
	}
	if n <= 255 {
		grey := 255 * (n - 232) / 23
		return fmt.Sprintf("#%02x%02x%02x", grey, grey, grey)
	}
	return "white"
}

// GetCached returns cached image info for a message ID.
func GetCached(messageID string) (CachedImage, bool) {
	cacheMu.RLock()
	defer cacheMu.RUnlock()
	v, ok := cache[messageID]
	return v, ok
}

// Store caches image info for a message ID.
func Store(messageID string, ci CachedImage) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cache[messageID] = ci
}

func getCacheFilePath() string {
	if p, err := xdg.ConfigFile("whatscli/imagecache.json"); err == nil {
		return p
	}
	return ""
}

// SaveCache persists the image cache to disk (file paths only, no art/PNG).
func SaveCache() {
	path := getCacheFilePath()
	if path == "" {
		return
	}
	cacheMu.RLock()
	snap := make(map[string]CachedImage, len(cache))
	for k, v := range cache {
		snap[k] = CachedImage{
			FilePath:        v.FilePath,
			PlaceholderRows: v.PlaceholderRows,
		}
	}
	cacheMu.RUnlock()

	if len(snap) > maxDiskCacheImages {
		trimmed := make(map[string]CachedImage, maxDiskCacheImages)
		i := 0
		for k, v := range snap {
			trimmed[k] = v
			i++
			if i >= maxDiskCacheImages {
				break
			}
		}
		snap = trimmed
	}

	data, err := json.Marshal(snap)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

// LoadCache restores the image cache from disk.
func LoadCache() {
	path := getCacheFilePath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var entries map[string]CachedImage
	if err := json.Unmarshal(data, &entries); err != nil {
		return
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	for k, v := range entries {
		cache[k] = v
	}
}

// ClearCache removes all entries from the in-memory cache.
func ClearCache() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cache = make(map[string]CachedImage)
}
