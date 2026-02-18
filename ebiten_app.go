package main

import (
	"fmt"
	"image"
	"image/color"
	"log"
	"math"
	"os"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/ebitenutil"
	"github.com/sqweek/dialog"
)

type Game struct {
	img    *image.RGBA
	w, h   int
	lastTs time.Time
	// UI controls
	baseline float64 // dB offset applied to PSD values
	gain     float64 // multiplicative gain applied after normalization
	// dragging state: "" | "baseline" | "gain"
	dragging string
	// cached static spectrogram recolored with current baseline/gain
	cachedStatic *ebiten.Image
	lastBaseline float64
	lastGain     float64

	// runtime GUI state for selecting mode and wav
	uiMode    string // "http", "ebiten", "both"
	uiAddr    string
	uiWavFile string
	uiWavMode string // "stream" or "static"
	// simple wav file list for quick browsing
	wavList  []string
	wavIndex int
	// input flag for typing into wav file field
	inputActive bool
	// previous mouse left state (for click detection)
	prevLeft bool
	// previous backspace key state
	prevBackspace bool
	// dropdown open state for mode selection
	modeOpen bool
}

// channel used to receive file path chosen by native dialog
var wavSelectCh = make(chan string, 1)

const controlPanelHeight = 96

const (
	displayFmin = 0.0    // Hz
	displayFmax = 3000.0 // Hz
)

// runEbiten starts the Ebiten UI and blocks until exit.
func runEbiten() {
	// Width = time axis, window height includes control panel on top
	w, winH := 800, 640
	specH := winH - controlPanelHeight
	g := &Game{
		img: image.NewRGBA(image.Rect(0, 0, w, specH)),
		w:   w,
		h:   specH,
		// sensible defaults so UI behaves immediately
		baseline:     0.0,
		gain:         1.0,
		lastBaseline: 1e9, // force initial rebuild of cached static
		lastGain:     -1e9,
		uiMode:       "both",
		uiAddr:       ":8080",
		uiWavFile:    "",
		uiWavMode:    "stream",
		wavList:      listWavFiles("MSK144_wav"),
	}

	ebiten.SetWindowSize(w, winH)
	ebiten.SetWindowTitle("Periodogram (Ebiten)")

	if err := ebiten.RunGame(g); err != nil {
		log.Fatalf("Ebiten run: %v", err)
	}
}

// Update reads latest PSD frames from psdCh (non-blocking) and updates the image.
func (g *Game) Update() error {
	// Drain channel and keep only latest frame
	var psd []float64
	for {
		select {
		case p := <-psdCh:
			psd = p
		default:
			goto DONE
		}
	}
DONE:
	hasPsd := psd != nil

	// Slider interaction handled later once panel coordinates are known.

	// GUI panel interaction (mode, wav file, apply/stop) - use click detection
	mx, my := ebiten.CursorPosition()
	leftPressed := ebiten.IsMouseButtonPressed(ebiten.MouseButtonLeft)
	clicked := leftPressed && !g.prevLeft
	// panel geometry (top full-width)
	px := 8
	py := 8
	// slider geometry: place sliders inside the top control panel
	sx := px + 8
	sw := 200
	bh := 12
	baseY := py + 8
	gainY := py + 32
	// Handle slider interaction (mouse) using panel coords
	left := ebiten.IsMouseButtonPressed(ebiten.MouseButtonLeft)
	if left {
		if g.dragging == "" {
			// start drag if cursor near either knob
			bx := sx + int((g.baseline+50.0)/100.0*float64(sw))
			gx := sx + int((math.Log10(g.gain)-math.Log10(0.1))/(math.Log10(10)-math.Log10(0.1))*float64(sw))
			if my >= baseY-4 && my <= baseY+bh {
				if abs(mx-bx) <= 8 || (mx >= sx && mx <= sx+sw) {
					g.dragging = "baseline"
				}
			}
			if my >= gainY-4 && my <= gainY+bh {
				if abs(mx-gx) <= 8 || (mx >= sx && mx <= sx+sw) {
					g.dragging = "gain"
				}
			}
		}
		// continue dragging
		if g.dragging == "baseline" {
			rel := clampF(float64(mx-sx)/float64(sw), 0, 1)
			g.baseline = rel*100.0 - 50.0
		} else if g.dragging == "gain" {
			rel := clampF(float64(mx-sx)/float64(sw), 0, 1)
			lg := math.Log10(0.1) + rel*(math.Log10(10)-math.Log10(0.1))
			g.gain = math.Pow(10, lg)
		}
	} else {
		g.dragging = ""
	}
	// Mode dropdown area
	modeLx := px + 8
	modeLy := py + 8
	modeRw := 16
	if clicked {
		// toggle mode dropdown if clicked on mode area
		if mx >= modeLx && mx <= modeLx+modeRw && my >= modeLy && my <= modeLy+modeRw {
			g.modeOpen = !g.modeOpen
		}
		// if dropdown open, check choices
		if g.modeOpen {
			opts := []string{"http", "ebiten", "both"}
			optX := modeLx
			optY := modeLy + modeRw + 4
			optW := 80
			optH := 18
			for i, o := range opts {
				oy := optY + i*(optH+2)
				if mx >= optX && mx <= optX+optW && my >= oy && my <= oy+optH {
					g.uiMode = o
					g.modeOpen = false
				}
			}
		}
		// browse wav list button -> open native file dialog
		browseX := px + 120
		browseY := py + 52
		if mx >= browseX && mx <= browseX+80 && my >= browseY && my <= browseY+20 {
			go func() {
				path, err := dialog.File().Filter("WAV", "wav").Title("Select WAV file").Load()
				if err == nil && path != "" {
					select {
					case wavSelectCh <- path:
					default:
					}
				}
			}()
		}
		// wav file input click toggles typing
		inputX := px + 8
		inputY := py + 72
		if mx >= inputX && mx <= inputX+200 && my >= inputY && my <= inputY+18 {
			g.inputActive = !g.inputActive
		} else {
			// click outside deactivates typing
			g.inputActive = false
		}
		// Apply and Stop buttons
		applyX := px + 420
		applyY := py + 72
		if mx >= applyX && mx <= applyX+80 && my >= applyY && my <= applyY+20 {
			startProcessing(g.uiMode, g.uiAddr, g.uiWavFile, g.uiWavMode)
		}
		stopX := px + 508
		if mx >= stopX && mx <= stopX+80 && my >= applyY && my <= applyY+20 {
			stopProcessing()
		}
	}
	// update prevLeft for next frame
	g.prevLeft = leftPressed

	// typing into wav file field
	if g.inputActive {
		chars := ebiten.InputChars()
		for _, r := range chars {
			if r == '\r' || r == '\n' {
				// ignore enter
				continue
			}
			g.uiWavFile += string(r)
		}
		back := ebiten.IsKeyPressed(ebiten.KeyBackspace)
		if back && !g.prevBackspace {
			if len(g.uiWavFile) > 0 {
				g.uiWavFile = g.uiWavFile[:len(g.uiWavFile)-1]
			}
		}
		g.prevBackspace = back
	}

	// receive any selected wav path from native dialog
	select {
	case p := <-wavSelectCh:
		g.uiWavFile = p
	default:
	}

	// Shift image left by 1 pixel (only when we have new PSD data)
	if hasPsd {
		// Copy each row's pixels left by 4 bytes per pixel
		for y := 0; y < g.h; y++ {
			rowStart := y * g.img.Stride
			// move bytes for pixels [1..w-1] to [0..w-2]
			copy(g.img.Pix[rowStart:rowStart+(g.w-1)*4], g.img.Pix[rowStart+4:rowStart+g.w*4])
		}
		// Fill rightmost column with new data limited to displayFmin..displayFmax.
		// PSD is in dB (negative values).
		bins := len(psd)
		// compute bin indices for display range
		binMin := int(math.Floor(displayFmin * float64(frameSize) / float64(sampleRate)))
		binMax := int(math.Ceil(displayFmax * float64(frameSize) / float64(sampleRate)))
		if binMin < 0 {
			binMin = 0
		}
		if binMax > bins-1 {
			binMax = bins - 1
		}

		// choose scaling range: prefer global precomputed values (from WAV) if available,
		// otherwise fall back to per-frame min/max.
		minDb := displayMinDbGlobal
		maxDb := displayMaxDbGlobal
		if !(minDb < maxDb) {
			// compute per-frame min/max
			minDb = math.Inf(1)
			maxDb = math.Inf(-1)
			for i := binMin; i <= binMax; i++ {
				if psd[i] < minDb {
					minDb = psd[i]
				}
				if psd[i] > maxDb {
					maxDb = psd[i]
				}
			}
			// fallback if invalid
			if math.IsInf(minDb, 1) || math.IsInf(maxDb, -1) {
				minDb = -100
				maxDb = 0
			}
		}

		// map pixels vertically to selected bin range.
		// We'll compute binF increasing with y so that after writing to flippedY
		// (g.h-1-y) the low frequencies appear at the bottom matching the labels.
		binRange := float64(binMax - binMin)
		for y := 0; y < g.h; y++ {
			// y: 0..h-1 top->bottom; binF increases top->bottom from binMin->binMax
			binF := float64(y)*binRange/float64(g.h-1) + float64(binMin)

			// average a small neighborhood for smoother display
			start := int(math.Floor(binF))
			if start < binMin {
				start = binMin
			}
			end := int(math.Min(float64(binMax+1), float64(start+1)))
			if start > binMax {
				start = binMax
				end = binMax + 1
			}
			sum := 0.0
			count := 0
			for i := start; i < end; i++ {
				sum += psd[i]
				count++
			}
			val := sum / math.Max(1, float64(count))

			// rescale using observed min/max in display range, apply baseline and gain
			var t float64
			valAdj := val + g.baseline
			if maxDb > minDb {
				t = (valAdj - minDb) / (maxDb - minDb)
			} else {
				// fallback to fixed mapping
				t = (valAdj + 100.0) / 100.0
			}
			t *= g.gain
			if t < 0 {
				t = 0
			}
			if t > 1 {
				t = 1
			}

			// convert to color (simple grayscale -> blueish)
			c := colormap(t)
			// set pixel at (w-1, flippedY)
			flippedY := g.h - 1 - y
			off := flippedY*g.img.Stride + (g.w-1)*4
			g.img.Pix[off+0] = c.R
			g.img.Pix[off+1] = c.G
			g.img.Pix[off+2] = c.B
			g.img.Pix[off+3] = 0xff
		}
	}

	g.lastTs = time.Now()
	return nil
}

// Draw renders the image to the screen.
func (g *Game) Draw(screen *ebiten.Image) {
	// If a static spectrogram image exists (precomputed from WAV), draw a
	// recolored cached version that respects the current baseline/gain. We
	// rebuild the cached image only when sliders change to avoid per-frame
	// allocations.
	if staticSpectrogram != nil {
		rebuild := false
		if g.cachedStatic == nil {
			rebuild = true
		}
		if g.baseline != g.lastBaseline || g.gain != g.lastGain {
			rebuild = true
		}

		if rebuild {
			// If PSD frames are available, recolor from them; otherwise fall
			// back to the precomputed staticSpectrogram image.
			if psdFramesGlobal != nil {
				b := staticSpectrogram.Bounds()
				w := b.Dx()
				h := b.Dy()
				img := image.NewRGBA(image.Rect(0, 0, w, h))

				bins := len(psdFramesGlobal[0])
				binMin := int(math.Floor(displayFmin * float64(frameSize) / float64(sampleRate)))
				binMax := int(math.Ceil(displayFmax * float64(frameSize) / float64(sampleRate)))
				if binMin < 0 {
					binMin = 0
				}
				if binMax > bins-1 {
					binMax = bins - 1
				}
				binRange := float64(binMax - binMin)

				for x := 0; x < w; x++ {
					p := psdFramesGlobal[x]
					for y := 0; y < h; y++ {
						// map y to bin (flip so bottom=low freq)
						rel := float64(h-1-y) / float64(h-1)
						binF := float64(binMin) + rel*binRange
						idx := int(math.Floor(binF))
						if idx < binMin {
							idx = binMin
						}
						if idx > binMax {
							idx = binMax
						}
						val := p[idx]
						// apply baseline and gain before normalization
						valAdj := val + g.baseline
						var t float64
						if displayMaxDbGlobal > displayMinDbGlobal {
							t = (valAdj - displayMinDbGlobal) / (displayMaxDbGlobal - displayMinDbGlobal)
						} else {
							t = (valAdj + 100.0) / 100.0
						}
						t *= g.gain
						if t < 0 {
							t = 0
						}
						if t > 1 {
							t = 1
						}
						col := colormap(t)
						img.Set(x, y, col)
					}
				}
				g.cachedStatic = ebiten.NewImageFromImage(img)
			} else {
				g.cachedStatic = ebiten.NewImageFromImage(staticSpectrogram)
			}
			g.lastBaseline = g.baseline
			g.lastGain = g.gain
		}

		// Draw the cached/recolored image scaled to the window
		op := &ebiten.DrawImageOptions{}
		sw := float64(g.cachedStatic.Bounds().Dx())
		sh := float64(g.cachedStatic.Bounds().Dy())
		if sw > 0 && sh > 0 {
			sx := float64(g.w) / sw
			sy := float64(g.h) / sh
			op.GeoM.Scale(sx, sy)
			// translate down by control panel height so spectrogram sits below controls
			op.GeoM.Translate(0, float64(controlPanelHeight))
		}
		screen.DrawImage(g.cachedStatic, op)
	} else {
		img := ebiten.NewImageFromImage(g.img)
		op := &ebiten.DrawImageOptions{}
		// translate down by control panel height so spectrogram sits below controls
		op.GeoM.Translate(0, float64(controlPanelHeight))
		screen.DrawImage(img, op)
	}
	// optional: draw FPS
	ebitenutil.DebugPrint(screen, "Press Ctrl+C to quit")

	// Draw control panel (top full-width)
	px := float64(0)
	py := float64(0)
	panelW := float64(g.w)
	panelH := float64(controlPanelHeight)
	ebitenutil.DrawRect(screen, px, py, panelW, panelH, color.RGBA{0x22, 0x22, 0x22, 0xd0})

	// Draw sliders inside panel
	sx := px + 8
	sw := 200.0
	baseY := py + 8
	gainY := py + 32
	ebitenutil.DrawRect(screen, sx, baseY, sw, 12, color.RGBA{0x33, 0x33, 0x33, 0xff})
	bx := sx + (g.baseline+50.0)/100.0*sw
	ebitenutil.DrawRect(screen, bx-6, baseY-2, 12, 16, color.RGBA{0x00, 0x00, 0x00, 0xff})
	ebitenutil.DebugPrintAt(screen, fmt.Sprintf("Baseline: %.1f dB", g.baseline), int(sx+sw+8), int(baseY))
	ebitenutil.DrawRect(screen, sx, gainY, sw, 12, color.RGBA{0x33, 0x33, 0x33, 0xff})
	lg := (math.Log10(g.gain) - math.Log10(0.1)) / (math.Log10(10) - math.Log10(0.1))
	gx := sx + lg*sw
	ebitenutil.DrawRect(screen, gx-6, gainY-2, 12, 16, color.RGBA{0x00, 0x00, 0x00, 0xff})
	ebitenutil.DebugPrintAt(screen, fmt.Sprintf("Gain: %.2fx", g.gain), int(sx+sw+8), int(gainY))

	// Mode label and dropdown button (next row)
	mlabelY := py + 52
	ebitenutil.DebugPrintAt(screen, fmt.Sprintf("Mode: %s", g.uiMode), int(px+36), int(mlabelY))
	ebitenutil.DrawRect(screen, px+8, mlabelY, 16, 16, color.RGBA{0x66, 0x66, 0x66, 0xff})
	if g.modeOpen {
		opts := []string{"http", "ebiten", "both"}
		for i, o := range opts {
			oy := mlabelY + 16 + 4 + float64(i*(18+2))
			ebitenutil.DrawRect(screen, px+8, oy, 80, 18, color.RGBA{0x44, 0x44, 0x44, 0xff})
			ebitenutil.DebugPrintAt(screen, o, int(px+12), int(oy+2))
		}
	}

	// Browse button
	// Browse button (to the right of mode label)
	ebitenutil.DrawRect(screen, px+120, mlabelY, 80, 20, color.RGBA{0x33, 0x99, 0xff, 0xff})
	ebitenutil.DebugPrintAt(screen, "Browse WAV...", int(px+124), int(mlabelY+2))
	// WAV input field (next row)
	ebitenutil.DrawRect(screen, px+8, py+72, 400, 18, color.RGBA{0x11, 0x11, 0x11, 0xff})
	ebitenutil.DebugPrintAt(screen, g.uiWavFile, int(px+12), int(py+72))

	// Apply/Stop buttons (aligned right)
	ebitenutil.DrawRect(screen, px+420, py+72, 80, 20, color.RGBA{0x22, 0xcc, 0x22, 0xff})
	ebitenutil.DebugPrintAt(screen, "Apply", int(px+424), int(py+74))
	ebitenutil.DrawRect(screen, px+508, py+72, 80, 20, color.RGBA{0xcc, 0x22, 0x22, 0xff})
	ebitenutil.DebugPrintAt(screen, "Stop", int(px+512), int(py+74))

	// Draw calibrated frequency labels on the left vertical axis at 500 Hz intervals.
	stepHz := 500.0
	// Precompute bin range mapping same as Update
	bins := frameSize / 2
	binMin := int(math.Floor(displayFmin * float64(frameSize) / float64(sampleRate)))
	binMax := int(math.Ceil(displayFmax * float64(frameSize) / float64(sampleRate)))
	if binMin < 0 {
		binMin = 0
	}
	if binMax > bins-1 {
		binMax = bins - 1
	}
	binRange := float64(binMax - binMin)

	for f := displayFmin; f <= displayFmax+1e-9; f += stepHz {
		// map frequency -> fractional bin
		binF := f * float64(frameSize) / float64(sampleRate)
		// relative position within displayed bin range (0..1 top->bottom)
		var rel float64
		if binRange > 0 {
			rel = (binF - float64(binMin)) / binRange
		} else {
			rel = 0
		}

		if rel < 0 {
			rel = 0
		}
		if rel > 1 {
			rel = 1
		}
		// flip so 0 Hz (rel=0) maps to bottom of image
		y := g.h - 1 - int(rel*float64(g.h-1))

		label := formatHz(f)

		// draw small tick mark as a filled rect to ensure solid black appearance
		// offset by control panel height so labels sit beside the spectrogram
		oy := float64(y) + float64(controlPanelHeight)
		ebitenutil.DrawRect(screen, 0, oy, 6, 1, color.Black)
		// draw a small black background behind the label for readability
		ebitenutil.DrawRect(screen, 6, oy-8, 80, 16, color.Black)
		// draw label (DebugPrintAt uses white text)
		ebitenutil.DebugPrintAt(screen, label, 8, int(oy-8))
	}

	// WAV mode
	ebitenutil.DebugPrintAt(screen, fmt.Sprintf("WavMode: %s", g.uiWavMode), int(px+8), int(py+52))
}

func (g *Game) Layout(outsideWidth, outsideHeight int) (int, int) { return g.w, g.h }

// simple colormap: map 0..1 to bluish->yellowish
func colormap(t float64) color.RGBA {
	// use a simple jet-like map
	r := uint8(math.Max(0, math.Min(255, 255*(t*2-0.5))))
	g := uint8(math.Max(0, math.Min(255, 255*(1-math.Abs(t-0.5)*2))))
	b := uint8(math.Max(0, math.Min(255, 255*(1-t))))
	return color.RGBA{r, g, b, 0xff}
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// formatHz formats frequency in Hz/kHz succinctly.
func formatHz(f float64) string {
	if f >= 1000 {
		return fmt.Sprintf("%.2f kHz", f/1000.0)
	}
	return fmt.Sprintf("%.0f Hz", f)
}

// listWavFiles returns .wav file names in the provided directory.
func listWavFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// simple suffix check
		if len(name) >= 4 && (name[len(name)-4:] == ".wav" || name[len(name)-4:] == ".WAV") {
			out = append(out, dir+"/"+name)
		}
	}
	return out
}
