package converter

import (
	"context"
	"runtime"
	"time"
)

const (
	defaultConversionDPI    = 300
	defaultThumbnailDPI     = 72
	defaultJPEGQuality      = 88
	defaultMaxPages         = 2000
	defaultMaxPixelsPerPage = int64(64_000_000)
	defaultMaxTotalPixels   = int64(2_000_000_000)
	defaultMinConversionDPI = 100
	defaultMaxOutputBytes   = int64(8 << 30)
	defaultMaxToolOutput    = int64(64 << 20)
	defaultPageTimeout      = 30 * time.Second
	maxRenderConcurrency    = 4
)

// OCRConfig controls the fallback used for pages without embedded text.
// Tesseract is discovered automatically when Binary is empty. A custom Run
// function can be supplied by embedders and tests.
type OCRConfig struct {
	Disabled     bool
	Required     bool
	Binary       string
	Language     string
	MinTextRunes int
	Timeout      time.Duration
	Run          OCRFunc
}

// OCRFunc performs OCR for one rendered page.
type OCRFunc func(ctx context.Context, imagePath, language string) (string, error)

// ToolPaths allows installations with non-standard Poppler locations to be
// explicit. Empty values use the executable names on PATH.
type ToolPaths struct {
	PDFInfo   string
	PDFToPPM  string
	PDFToText string
	PDFToHTML string
}

// Options controls conversion fidelity and resource limits. Zero values use
// DefaultOptions. Negative quota values disable that quota.
type Options struct {
	ConversionDPI    int
	ThumbnailDPI     int
	JPEGQuality      int
	Timeout          time.Duration
	MaxPages         int
	MaxPixelsPerPage int64
	MaxTotalPixels   int64
	// MinConversionDPI is the lowest DPI the converter may fall back to when a
	// document would exceed MaxTotalPixels at ConversionDPI.
	MinConversionDPI   int
	MaxOutputBytes     int64
	MaxToolOutputBytes int64
	OCR                OCRConfig
	Tools              ToolPaths

	// PageTimeout is added to Timeout for every page once the page count is
	// known, so long documents are not held to the same budget as a short
	// deck. Negative keeps the base Timeout for every document.
	PageTimeout time.Duration
	// RenderConcurrency is the number of pages rendered at once. Each
	// in-flight page holds one Poppler process and one decoded page bitmap in
	// memory (about 150 MB for a 300 DPI double-page spread).
	RenderConcurrency int
}

// DefaultOptions returns the production defaults used by New.
func DefaultOptions() Options {
	return Options{
		ConversionDPI:      defaultConversionDPI,
		ThumbnailDPI:       defaultThumbnailDPI,
		JPEGQuality:        defaultJPEGQuality,
		Timeout:            10 * time.Minute,
		PageTimeout:        defaultPageTimeout,
		RenderConcurrency:  defaultRenderConcurrency(),
		MaxPages:           defaultMaxPages,
		MaxPixelsPerPage:   defaultMaxPixelsPerPage,
		MaxTotalPixels:     defaultMaxTotalPixels,
		MinConversionDPI:   defaultMinConversionDPI,
		MaxOutputBytes:     defaultMaxOutputBytes,
		MaxToolOutputBytes: defaultMaxToolOutput,
		OCR: OCRConfig{
			Language:     "eng",
			MinTextRunes: 1,
			Timeout:      45 * time.Second,
		},
		Tools: ToolPaths{
			PDFInfo:   "pdfinfo",
			PDFToPPM:  "pdftoppm",
			PDFToText: "pdftotext",
			PDFToHTML: "pdftohtml",
		},
	}
}

func normalizeOptions(opts Options) Options {
	defaults := DefaultOptions()
	if opts.ConversionDPI == 0 {
		opts.ConversionDPI = defaults.ConversionDPI
	}
	if opts.ThumbnailDPI == 0 {
		opts.ThumbnailDPI = defaults.ThumbnailDPI
	}
	if opts.JPEGQuality == 0 {
		opts.JPEGQuality = defaults.JPEGQuality
	}
	if opts.Timeout == 0 {
		opts.Timeout = defaults.Timeout
	}
	if opts.PageTimeout == 0 {
		opts.PageTimeout = defaults.PageTimeout
	}
	if opts.RenderConcurrency == 0 {
		opts.RenderConcurrency = defaults.RenderConcurrency
	}
	if opts.MaxPages == 0 {
		opts.MaxPages = defaults.MaxPages
	}
	if opts.MaxPixelsPerPage == 0 {
		opts.MaxPixelsPerPage = defaults.MaxPixelsPerPage
	}
	if opts.MaxTotalPixels == 0 {
		opts.MaxTotalPixels = defaults.MaxTotalPixels
	}
	if opts.MinConversionDPI == 0 {
		opts.MinConversionDPI = defaults.MinConversionDPI
	}
	if opts.MaxOutputBytes == 0 {
		opts.MaxOutputBytes = defaults.MaxOutputBytes
	}
	if opts.MaxToolOutputBytes == 0 {
		opts.MaxToolOutputBytes = defaults.MaxToolOutputBytes
	}
	if opts.OCR.Language == "" {
		opts.OCR.Language = defaults.OCR.Language
	}
	if opts.OCR.MinTextRunes == 0 {
		opts.OCR.MinTextRunes = defaults.OCR.MinTextRunes
	}
	if opts.OCR.Timeout == 0 {
		opts.OCR.Timeout = defaults.OCR.Timeout
	}
	if opts.Tools.PDFInfo == "" {
		opts.Tools.PDFInfo = defaults.Tools.PDFInfo
	}
	if opts.Tools.PDFToPPM == "" {
		opts.Tools.PDFToPPM = defaults.Tools.PDFToPPM
	}
	if opts.Tools.PDFToText == "" {
		opts.Tools.PDFToText = defaults.Tools.PDFToText
	}
	if opts.Tools.PDFToHTML == "" {
		opts.Tools.PDFToHTML = defaults.Tools.PDFToHTML
	}
	return opts
}

func validateOptions(opts Options) error {
	if opts.ConversionDPI < 1 || opts.ThumbnailDPI < 1 || opts.MinConversionDPI < 1 {
		return newConversionError(ErrInvalidOptions, "dpi must be positive", nil)
	}
	if opts.JPEGQuality < 1 || opts.JPEGQuality > 100 {
		return newConversionError(ErrInvalidOptions, "jpeg quality must be between 1 and 100", nil)
	}
	if opts.MaxPages > 1_000_000 {
		return newConversionError(ErrInvalidOptions, "max pages must be negative (unlimited) or between 1 and 1000000", nil)
	}
	if opts.OCR.MinTextRunes < 0 {
		return newConversionError(ErrInvalidOptions, "OCR minimum text runes cannot be negative", nil)
	}
	if opts.RenderConcurrency < 1 || opts.RenderConcurrency > 64 {
		return newConversionError(ErrInvalidOptions, "render concurrency must be between 1 and 64", nil)
	}
	return nil
}

// defaultRenderConcurrency uses the CPUs available to the process (Go sizes
// GOMAXPROCS to the container's CPU quota), capped to keep peak memory of
// concurrent 300 DPI page bitmaps bounded.
func defaultRenderConcurrency() int {
	n := runtime.GOMAXPROCS(0)
	if n > maxRenderConcurrency {
		n = maxRenderConcurrency
	}
	if n < 1 {
		n = 1
	}
	return n
}
