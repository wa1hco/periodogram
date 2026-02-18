package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"log"
	"math"
	"math/cmplx"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/go-audio/wav"
	"github.com/gorilla/websocket"
	"github.com/mesilliac/pulse-simple"
)

// make core audio params writable so WAV mode can override sampleRate
var (
	sampleRate = 44100
	// Reduce frameSize to lower latency. frameSize=2048 => 46.4 ms window at 44.1kHz.
	frameSize = 2048 // FFT size — tradeoff: lower = less freq resolution
	hopSize   = 512  // overlap-add hop
)

// global display scaling values (set when WAV pre-processing computes noise floor)
var (
	displayMinDbGlobal = math.Inf(1)
	displayMaxDbGlobal = math.Inf(-1)
	staticSpectrogram  image.Image
	psdFramesGlobal    [][]float64
)

// autoscaleSizes chooses frameSize (power of two) and hopSize to target
// approximately the same analysis window and hop durations across sample rates.
func autoscaleSizes(sr int) {
	// target durations (ms)
	const targetWindowMs = 50.0
	const targetHopMs = 12.0

	// compute desired frame in samples, then round up to next power of two
	desired := int(math.Ceil(float64(sr) * targetWindowMs / 1000.0))
	// enforce reasonable bounds
	if desired < 128 {
		desired = 128
	}
	if desired > 16384 {
		desired = 16384
	}
	frameSize = nextPow2(desired)

	// compute hop in samples (at least 16), but keep it <= frameSize/2
	hop := int(math.Round(float64(sr) * targetHopMs / 1000.0))
	if hop < 16 {
		hop = 16
	}
	if hop > frameSize/2 {
		hop = frameSize / 4
		if hop < 16 {
			hop = 16
		}
	}
	hopSize = hop

	log.Printf("autoscale: sr=%d frameSize=%d hopSize=%d window=%.1fms hop=%.1fms",
		sr, frameSize, hopSize, float64(frameSize)/float64(sr)*1000.0, float64(hopSize)/float64(sr)*1000.0)
}

func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// ---------------------------------------------------------------------------
// Window functions
// ---------------------------------------------------------------------------

// hann returns a Hann window of length n.
func hann(n int) []float64 {
	w := make([]float64, n)
	for i := range w {
		w[i] = 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/float64(n-1)))
	}
	return w
}

// ---------------------------------------------------------------------------
// FFT (Cooley–Tukey, radix-2, in-place)
// ---------------------------------------------------------------------------

func fft(x []complex128) {
	n := len(x)
	if n <= 1 {
		return
	}
	// bit-reversal permutation
	j := 0
	for i := 1; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}
	// Cooley–Tukey
	for length := 2; length <= n; length <<= 1 {
		angle := -2 * math.Pi / float64(length)
		wlen := complex(math.Cos(angle), math.Sin(angle))
		for i := 0; i < n; i += length {
			w := complex(1, 0)
			for k := 0; k < length/2; k++ {
				u := x[i+k]
				v := x[i+k+length/2] * w
				x[i+k] = u + v
				x[i+k+length/2] = u - v
				w *= wlen
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Periodogram computation
// ---------------------------------------------------------------------------

// periodogram applies a Hann window to samples and returns the power
// spectral density in dB (normalised to 0 dB peak).
func periodogram(samples []float32) []float64 {
	n := len(samples)
	win := hann(n)

	cx := make([]complex128, n)
	for i, s := range samples {
		cx[i] = complex(float64(s)*win[i], 0)
	}

	fft(cx)

	psd := make([]float64, n/2)
	for i := range psd {
		psd[i] = cmplx.Abs(cx[i]) * cmplx.Abs(cx[i])
	}
	// Convert to absolute dB (reference = 1.0 power unit). Avoid log(0).
	// Do NOT normalise to the frame peak here; downstream code will scale
	// frames based on a measured noise floor (global or per-frame).
	eps := 1e-20
	for i := range psd {
		db := 10 * math.Log10(psd[i]+eps)
		// clamp to a reasonable floor to avoid -Inf
		if db < -200 {
			db = -200
		}
		psd[i] = db
	}
	return psd
}

// ---------------------------------------------------------------------------
// WebSocket hub
// ---------------------------------------------------------------------------

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// psdCh receives PSD frames for local (non-HTTP) visualization.
var psdCh = make(chan []float64, 4)

// Managed runtime state so the Ebiten GUI can start/stop capture and HTTP server
var (
	hubGlobal    *Hub
	captureDone  chan struct{}
	httpServer   *http.Server
	httpShutdown chan struct{}
)

type Hub struct {
	mu      sync.Mutex
	clients map[*websocket.Conn]bool
}

func newHub() *Hub { return &Hub{clients: make(map[*websocket.Conn]bool)} }

func (h *Hub) add(c *websocket.Conn) {
	h.mu.Lock()
	h.clients[c] = true
	h.mu.Unlock()
}

func (h *Hub) remove(c *websocket.Conn) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

func (h *Hub) broadcast(msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if err := c.WriteMessage(websocket.TextMessage, msg); err != nil {
			c.Close()
			delete(h.clients, c)
		}
	}
}

// ---------------------------------------------------------------------------
// Audio capture loop
// ---------------------------------------------------------------------------

func captureAudio(hub *Hub, done <-chan struct{}) {
	ss := pulse.SampleSpec{
		Format:   pulse.SAMPLE_FLOAT32LE,
		Rate:     uint32(sampleRate),
		Channels: 1,
	}

	stream, err := pulse.Capture("periodogram", "Audio capture", &ss)
	if err != nil {
		log.Fatalf("PulseAudio capture: %v", err)
	}
	defer stream.Free()

	log.Println("PulseAudio capture started")

	buf := make([]float32, hopSize)
	ring := make([]float32, frameSize)
	ringPos := 0

	ticker := time.NewTicker(time.Duration(hopSize) * time.Second / time.Duration(sampleRate))
	defer ticker.Stop()

	// Create byte buffer for reading from PulseAudio
	byteBuf := make([]byte, hopSize*4) // 4 bytes per float32

	for {
		select {
		case <-done:
			log.Println("captureAudio: stopped")
			return
		case <-ticker.C:
		}
		// Read audio samples from PulseAudio
		n, err := stream.Read(byteBuf)
		if err != nil {
			log.Printf("PulseAudio read error: %v", err)
			continue
		}

		// Convert bytes to float32
		samplesRead := n / 4
		for i := 0; i < samplesRead; i++ {
			// Convert little-endian bytes to float32
			bits := uint32(byteBuf[i*4]) | uint32(byteBuf[i*4+1])<<8 |
				uint32(byteBuf[i*4+2])<<16 | uint32(byteBuf[i*4+3])<<24
			buf[i] = math.Float32frombits(bits)
		}

		// Fill circular ring buffer
		for i := 0; i < samplesRead; i++ {
			ring[ringPos%frameSize] = buf[i]
			ringPos++
		}

		// Build contiguous frame (oldest → newest)
		frame := make([]float32, frameSize)
		start := ringPos % frameSize
		for i := 0; i < frameSize; i++ {
			frame[i] = ring[(start+i)%frameSize]
		}

		psd := periodogram(frame)

		// Package as JSON: {bins: [...], sampleRate: 44100, frameSize: 4096}
		payload := map[string]interface{}{
			"bins":       psd,
			"sampleRate": sampleRate,
			"frameSize":  frameSize,
		}
		data, _ := json.Marshal(payload)
		hub.broadcast(data)

		// Also send to local PSD channel for in-process visualization (Ebiten)
		select {
		case psdCh <- psd:
		default:
		}
	}
}

// captureWavAudio reads samples from a WAV file and feeds the same processing
// pipeline as live capture, simulating real-time playback using a ticker.
func captureWavAudio(hub *Hub, path string, done <-chan struct{}) {
	f, err := os.Open(path)
	if err != nil {
		log.Fatalf("open wav: %v", err)
	}
	defer f.Close()

	dec := wav.NewDecoder(f)
	if !dec.IsValidFile() {
		log.Fatalf("invalid WAV file: %s", path)
	}

	// Read full PCM buffer
	buf, err := dec.FullPCMBuffer()
	if err != nil {
		log.Fatalf("decode wav: %v", err)
	}

	// Convert interleaved samples to mono by taking first channel
	nch := buf.Format.NumChannels
	totalFrames := len(buf.Data) / nch
	samples := make([]float32, totalFrames)
	for i := 0; i < totalFrames; i++ {
		samples[i] = float32(buf.Data[i*nch])
	}

	log.Printf("WAV: %s sr=%d frames=%d", path, sampleRate, totalFrames)

	// Precompute PSD frames for the entire file using current frameSize/hopSize.
	var psdFrames [][]float64
	// If static precompute was done earlier, use it; otherwise precompute now.
	if psdFramesGlobal != nil {
		psdFrames = psdFramesGlobal
	} else {
		psdFrames, displayMinDbGlobal, displayMaxDbGlobal = precomputeWavPSDs(samples, totalFrames)
		psdFramesGlobal = psdFrames
	}

	// Compute noise-floor and max over the displayed bin range across all frames.
	bins := len(psdFrames[0])
	binMin := int(math.Floor(displayFmin * float64(frameSize) / float64(sampleRate)))
	binMax := int(math.Ceil(displayFmax * float64(frameSize) / float64(sampleRate)))
	if binMin < 0 {
		binMin = 0
	}
	if binMax > bins-1 {
		binMax = bins - 1
	}

	// collect values for percentile/maximum
	vals := make([]float64, 0, len(psdFrames)*(binMax-binMin+1))
	var globalMax float64 = -1e9
	for _, p := range psdFrames {
		for i := binMin; i <= binMax; i++ {
			vals = append(vals, p[i])
			if p[i] > globalMax {
				globalMax = p[i]
			}
		}
	}
	if len(vals) > 0 {
		sort.Float64s(vals)
		// use 10th percentile as noise floor
		idx := int(math.Max(0, math.Floor(0.10*float64(len(vals)))))
		displayMinDbGlobal = vals[idx]
		displayMaxDbGlobal = globalMax
	} else {
		displayMinDbGlobal = -100
		displayMaxDbGlobal = 0
	}
	log.Printf("WAV noise-floor estimate: minDb=%.2f maxDb=%.2f", displayMinDbGlobal, displayMaxDbGlobal)

	// Stream the precomputed frames at real-time cadence (looping)
	ticker := time.NewTicker(time.Duration(hopSize) * time.Second / time.Duration(sampleRate))
	defer ticker.Stop()

	idx := 0
	for {
		select {
		case <-done:
			log.Println("captureWavAudio: stopped")
			return
		case <-ticker.C:
		}
		psd := psdFrames[idx%len(psdFrames)]

		payload := map[string]interface{}{
			"bins":       psd,
			"sampleRate": sampleRate,
			"frameSize":  frameSize,
		}
		data, _ := json.Marshal(payload)
		hub.broadcast(data)

		select {
		case psdCh <- psd:
		default:
		}

		idx++
	}
}

// startProcessing starts capture and optionally an HTTP server according to
// the given mode. Any previously running capture or HTTP server will be
// stopped first.
func startProcessing(modeStr, addr, wavfile, wavMode string) {
	// stop previous
	stopProcessing()

	if hubGlobal == nil {
		hubGlobal = newHub()
	}

	// start capture
	captureDone = make(chan struct{})
	if wavfile != "" {
		// If wav-mode==static and static spectrogram not built, build now
		if wavMode == "static" && staticSpectrogram == nil {
			log.Printf("Building static spectrogram for %s...", wavfile)
			if err := buildStaticSpectrogram(wavfile); err != nil {
				log.Printf("build static spectrogram: %v", err)
			} else {
				log.Printf("Static spectrogram ready")
			}
		}
		go captureWavAudio(hubGlobal, wavfile, captureDone)
	} else {
		go captureAudio(hubGlobal, captureDone)
	}

	// start HTTP server if mode requests it
	if modeStr == "http" || modeStr == "both" {
		httpServer = &http.Server{Addr: addr}
		httpShutdown = make(chan struct{})
		// register handlers using the hubGlobal
		http.HandleFunc("/ws", wsHandler(hubGlobal))
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, "index.html")
		})
		go func() {
			log.Printf("Periodogram server → http://%s", addr)
			if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("HTTP server: %v", err)
			}
			close(httpShutdown)
		}()
	}
}

// stopProcessing stops any running capture goroutine and HTTP server.
func stopProcessing() {
	if captureDone != nil {
		close(captureDone)
		captureDone = nil
	}
	if httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("HTTP shutdown: %v", err)
		}
		httpServer = nil
		// drain httpShutdown if needed
		if httpShutdown != nil {
			<-httpShutdown
			httpShutdown = nil
		}
	}
}

// precomputeWavPSDs computes PSD frames for the WAV samples and returns
// the PSDs plus estimated noise-floor (10th percentile) and global max
// within the display band.
func precomputeWavPSDs(samples []float32, totalFrames int) ([][]float64, float64, float64) {
	var psdFrames [][]float64
	if totalFrames < frameSize {
		frame := make([]float32, frameSize)
		copy(frame, samples)
		psdFrames = append(psdFrames, periodogram(frame))
	} else {
		for start := 0; start+frameSize <= totalFrames; start += hopSize {
			frame := make([]float32, frameSize)
			copy(frame, samples[start:start+frameSize])
			psd := periodogram(frame)
			psdFrames = append(psdFrames, psd)
		}
	}

	bins := len(psdFrames[0])
	binMin := int(math.Floor(displayFmin * float64(frameSize) / float64(sampleRate)))
	binMax := int(math.Ceil(displayFmax * float64(frameSize) / float64(sampleRate)))
	if binMin < 0 {
		binMin = 0
	}
	if binMax > bins-1 {
		binMax = bins - 1
	}

	vals := make([]float64, 0, len(psdFrames)*(binMax-binMin+1))
	var globalMax float64 = -1e9
	for _, p := range psdFrames {
		for i := binMin; i <= binMax; i++ {
			vals = append(vals, p[i])
			if p[i] > globalMax {
				globalMax = p[i]
			}
		}
	}
	if len(vals) == 0 {
		return psdFrames, -100, 0
	}
	sort.Float64s(vals)
	idx := int(math.Max(0, math.Floor(0.10*float64(len(vals)))))
	return psdFrames, vals[idx], globalMax
}

// buildStaticSpectrogram constructs an image from the WAV file PSDs and stores
// it in `staticSpectrogram` and `psdFramesGlobal` for reuse.
func buildStaticSpectrogram(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := wav.NewDecoder(f)
	if !dec.IsValidFile() {
		return fmt.Errorf("invalid WAV file: %s", path)
	}
	buf, err := dec.FullPCMBuffer()
	if err != nil {
		return err
	}
	nch := buf.Format.NumChannels
	totalFrames := len(buf.Data) / nch
	samples := make([]float32, totalFrames)
	for i := 0; i < totalFrames; i++ {
		samples[i] = float32(buf.Data[i*nch])
	}

	psdFrames, minDb, maxDb := precomputeWavPSDs(samples, totalFrames)
	psdFramesGlobal = psdFrames
	displayMinDbGlobal = minDb
	displayMaxDbGlobal = maxDb

	// Build an image where width = number of frames, height = 512
	h := 512
	w := len(psdFrames)
	if w <= 0 {
		return fmt.Errorf("no PSD frames")
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	bins := len(psdFrames[0])
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
		p := psdFrames[x]
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
			// scale using global min/max
			var t float64
			if displayMaxDbGlobal > displayMinDbGlobal {
				t = (val - displayMinDbGlobal) / (displayMaxDbGlobal - displayMinDbGlobal)
			} else {
				t = (val + 100.0) / 100.0
			}
			if t < 0 {
				t = 0
			}
			if t > 1 {
				t = 1
			}
			// color mapping using the same colormap as the rolling display
			col := colormap(t)
			img.Set(x, y, col)
		}
	}
	staticSpectrogram = img
	return nil
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func wsHandler(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		hub.add(conn)
		// drain any incoming messages (keep-alive)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				hub.remove(conn)
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	// CLI flags: --mode {http,ebiten,both} and --addr for HTTP listen address.
	mode := flag.String("mode", "both", "UI mode: http, ebiten, or both")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	wavfile := flag.String("wav", "", "path to WAV file to use instead of live capture")
	wavMode := flag.String("wav-mode", "stream", "wav mode: stream or static")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		flag.PrintDefaults()
	}

	flag.Parse()

	// If program invoked with no options, print help and exit.
	if len(os.Args) == 1 {
		flag.Usage()
		return
	}

	hubGlobal = newHub()

	// If a WAV file was provided, read its header now so we can set sampleRate
	// and autoscale frame/hop sizes before starting capture. Otherwise, leave
	// sampleRate default and autoscale for the live device.
	if *wavfile != "" {
		f, err := os.Open(*wavfile)
		if err != nil {
			log.Fatalf("open wav for header: %v", err)
		}
		dec := wav.NewDecoder(f)
		if !dec.IsValidFile() {
			f.Close()
			log.Fatalf("invalid WAV file: %s", *wavfile)
		}
		sampleRate = int(dec.SampleRate)
		f.Close()
	}

	// Autoscale sizes based on the selected sample rate
	autoscaleSizes(sampleRate)

	// If wav-mode==static, build static spectrogram now (startProcessing will
	// also build it if invoked later from GUI).
	if *wavfile != "" && *wavMode == "static" {
		log.Printf("Building static spectrogram for %s...", *wavfile)
		if err := buildStaticSpectrogram(*wavfile); err != nil {
			log.Fatalf("build static spectrogram: %v", err)
		}
		log.Printf("Static spectrogram ready")
	}

	// Start capture/server according to CLI flags. The Ebiten GUI can also
	// call startProcessing at runtime to change mode or WAV file.
	startProcessing(*mode, *addr, *wavfile, *wavMode)

	switch *mode {
	case "ebiten":
		// Run only local Ebiten UI; no blocking HTTP server here.
		runEbiten()
	case "http":
		// Server was started by startProcessing; block until it's done.
		log.Printf("Periodogram server → http://%s", *addr)
		select {}
	case "both":
		// HTTP server is running; run Ebiten UI locally as well.
		runEbiten()
	default:
		log.Fatalf("unknown mode: %s (use http, ebiten, or both)", *mode)
	}
}
