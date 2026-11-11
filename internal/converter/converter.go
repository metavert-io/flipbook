package converter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Result retains the original first-page fields and adds the complete
// per-page conversion manifest for revision-aware workers and viewers.
type Result struct {
	PageCount    int
	Width        int
	Height       int
	Pages        []PageManifest
	Warnings     []Warning
	PDFPath      string
	ManifestPath string
	StagingDir   string
}

type Converter struct {
	libreOfficeBin string
	tmpDir         string
	conversionDPI  int
	thumbnailDPI   int
	options        Options
	optionsErr     error
}

func New(libreOfficeBin, tmpDir string, conversionDPI, thumbnailDPI int) *Converter {
	options := DefaultOptions()
	options.ConversionDPI = conversionDPI
	options.ThumbnailDPI = thumbnailDPI
	return NewWithOptions(libreOfficeBin, tmpDir, options)
}

// NewWithOptions creates a converter with explicit resource, OCR, rendition,
// and Poppler settings. Invalid options are reported by Convert.
func NewWithOptions(libreOfficeBin, tmpDir string, options Options) *Converter {
	options = normalizeOptions(options)
	if tmpDir != "" {
		_ = os.MkdirAll(tmpDir, 0755)
	}
	return &Converter{
		libreOfficeBin: libreOfficeBin,
		tmpDir:         tmpDir,
		conversionDPI:  options.ConversionDPI,
		thumbnailDPI:   options.ThumbnailDPI,
		options:        options,
		optionsErr:     validateOptions(options),
	}
}

// Convert preserves the legacy output layout. Conversion happens in a sibling
// staging directory; existing live assets are replaced only after every new
// artifact validates, and replaced assets are restored if promotion fails.
func (c *Converter) Convert(ctx context.Context, srcPath, pagesDir, thumbsDir string) (*Result, error) {
	pagesDir, err := filepath.Abs(pagesDir)
	if err != nil {
		return nil, err
	}
	thumbsDir, err = filepath.Abs(thumbsDir)
	if err != nil {
		return nil, err
	}
	outDir := filepath.Dir(pagesDir)
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return nil, err
	}
	stagingDir, err := os.MkdirTemp(outDir, ".conversion-*")
	if err != nil {
		return nil, fmt.Errorf("create conversion staging: %w", err)
	}
	defer os.RemoveAll(stagingDir)

	result, err := c.convertStaged(ctx, srcPath, stagingDir, filepath.Base(pagesDir), filepath.Base(thumbsDir))
	if err != nil {
		return nil, err
	}
	originalName := "original" + strings.ToLower(filepath.Ext(srcPath))
	if err := promoteLegacyAssets(stagingDir, pagesDir, thumbsDir, originalName); err != nil {
		return nil, err
	}
	result.StagingDir = ""
	result.PDFPath = filepath.Join(outDir, "converted.pdf")
	result.ManifestPath = filepath.Join(outDir, "manifest.json")
	return result, nil
}

// ConvertStaged renders a complete revision into stagingDir using the standard
// pages, thumbs, and renditions layout. The caller owns staging cleanup and
// promotion; unsuccessful staging directories are left for diagnostics.
func (c *Converter) ConvertStaged(ctx context.Context, srcPath, stagingDir string) (*Result, error) {
	return c.convertStaged(ctx, srcPath, stagingDir, "pages", "thumbs")
}

func (c *Converter) convertStaged(ctx context.Context, srcPath, stagingDir, pagesBase, thumbsBase string) (*Result, error) {
	if c.optionsErr != nil {
		return nil, c.optionsErr
	}
	reserved := map[string]bool{"renditions": true, "text.json": true, "manifest.json": true, "converted.pdf": true, ".previous": true}
	if pagesBase == "" || thumbsBase == "" || pagesBase == thumbsBase || filepath.Base(pagesBase) != pagesBase || filepath.Base(thumbsBase) != thumbsBase || reserved[pagesBase] || reserved[thumbsBase] || strings.HasPrefix(pagesBase, "original.") || strings.HasPrefix(thumbsBase, "original.") {
		return nil, newConversionError(ErrInvalidStaging, "output layout", errors.New("pages and thumbnails must have different non-reserved directory names"))
	}
	limit := newTimeLimit(ctx, c.options.Timeout)
	defer limit.stop()
	result, err := c.convertDocument(limit, srcPath, stagingDir, pagesBase, thumbsBase)
	return result, limit.classify(err)
}

func (c *Converter) convertDocument(limit *timeLimit, srcPath, stagingDir, pagesBase, thumbsBase string) (*Result, error) {
	ctx := limit.ctx
	if err := ValidateSourceFormat(srcPath); err != nil {
		return nil, err
	}
	sourceInfo, err := os.Stat(srcPath)
	if err != nil {
		return nil, err
	}
	if !sourceInfo.Mode().IsRegular() || sourceInfo.Size() == 0 {
		return nil, errors.New("source document is empty or not a regular file")
	}
	stagingDir, err = filepath.Abs(stagingDir)
	if err != nil {
		return nil, err
	}
	if err := prepareStaging(stagingDir, pagesBase, thumbsBase); err != nil {
		return nil, err
	}

	extension := strings.ToLower(filepath.Ext(srcPath))
	originalName := "original" + extension
	originalPath := filepath.Join(stagingDir, originalName)
	if !samePath(srcPath, originalPath) {
		if _, err := copyFileAtomic(srcPath, originalPath); err != nil {
			return nil, fmt.Errorf("retain original: %w", err)
		}
	}

	pdfPath := filepath.Join(stagingDir, "converted.pdf")
	var warnings []Warning
	if extension == ".pdf" {
		if !samePath(srcPath, pdfPath) {
			if _, err := copyFileAtomic(srcPath, pdfPath); err != nil {
				return nil, fmt.Errorf("stage PDF: %w", err)
			}
		}
		if err := validatePDFFile(stagingDir, "converted.pdf"); err != nil {
			return nil, err
		}
	} else {
		var documentWarnings []Warning
		ReportProgress(ctx, Progress{Stage: ProgressConvertDocument})
		pdfPath, documentWarnings, err = c.documentToPDF(ctx, srcPath, stagingDir)
		warnings = append(warnings, documentWarnings...)
		if err != nil {
			return nil, err
		}
	}

	budget := &outputBudget{limit: c.options.MaxOutputBytes}
	if err := budget.add(originalPath); err != nil {
		return nil, err
	}
	if err := budget.add(pdfPath); err != nil {
		return nil, err
	}
	documentInfo, infoWarnings, err := c.inspectPDF(ctx, pdfPath)
	warnings = append(warnings, infoWarnings...)
	if err != nil {
		return nil, err
	}
	if c.options.MaxPages > 0 && documentInfo.pageCount > c.options.MaxPages {
		return nil, newConversionError(ErrQuotaExceeded, "page limit", fmt.Errorf("document has %d pages, limit is %d", documentInfo.pageCount, c.options.MaxPages))
	}
	ctx = limit.extendForPages(documentInfo.pageCount, c.options.PageTimeout)
	renderDPI, dpiWarning, err := c.fitRenderDPI(documentInfo.pages)
	if err != nil {
		return nil, err
	}
	if dpiWarning != nil {
		warnings = append(warnings, *dpiWarning)
	}

	absPDF, err := filepath.Abs(pdfPath)
	if err != nil {
		return nil, err
	}
	pages, renderWarnings, err := c.renderPages(ctx, pageRenderJob{
		pdfPath:    absPDF,
		stagingDir: stagingDir,
		pagesBase:  pagesBase,
		thumbsBase: thumbsBase,
		dpi:        renderDPI,
		geometry:   documentInfo.pages[:documentInfo.pageCount],
		budget:     budget,
	})
	warnings = append(warnings, renderWarnings...)
	if err != nil {
		return nil, err
	}
	pageImages := make([]string, len(pages))
	for i := range pages {
		pageImages[i] = filepath.Join(stagingDir, pagesBase, fmt.Sprintf("page-%d.png", pages[i].Number))
	}

	ReportProgress(ctx, Progress{Stage: ProgressExtractText})
	texts, positionedText, textWarnings := c.extractTextArtifacts(ctx, pdfPath, documentInfo.pageCount)
	warnings = append(warnings, textWarnings...)
	texts, ocrUsed, ocrWarnings, err := c.applyOCR(ctx, texts, pageImages)
	warnings = append(warnings, ocrWarnings...)
	if err != nil {
		return nil, err
	}
	links, outline, linkWarnings := c.extractLinksAndOutline(ctx, pdfPath)
	warnings = append(warnings, linkWarnings...)
	textAvailable := false
	for i := range pages {
		page := &pages[i]
		page.Text = positionedText[page.Number]
		page.Links = links[page.Number]
		if strings.TrimSpace(texts[i]) != "" {
			textAvailable = true
			page.TextSource = "poppler"
		}
		if ocrUsed[page.Number] {
			page.TextSource = "ocr"
		}
	}
	if err := writePageTexts(stagingDir, texts); err != nil {
		return nil, fmt.Errorf("write text.json: %w", err)
	}
	if err := budget.add(filepath.Join(stagingDir, "text.json")); err != nil {
		return nil, err
	}

	manifest := &Manifest{
		SchemaVersion: ManifestSchemaVersion,
		CreatedAt:     time.Now().UTC(),
		SourceFormat:  strings.TrimPrefix(extension, "."),
		SourcePDF:     "converted.pdf",
		Original:      originalName,
		ConversionDPI: renderDPI,
		ThumbnailDPI:  c.options.ThumbnailDPI,
		PageCount:     documentInfo.pageCount,
		Width:         pages[0].Width,
		Height:        pages[0].Height,
		TextAvailable: textAvailable,
		Pages:         pages,
		Outline:       outline,
		Warnings:      warnings,
	}
	if err := writeManifest(stagingDir, manifest); err != nil {
		return nil, fmt.Errorf("write manifest.json: %w", err)
	}
	if err := budget.add(filepath.Join(stagingDir, "manifest.json")); err != nil {
		return nil, err
	}
	if err := ValidateArtifacts(stagingDir, manifest); err != nil {
		return nil, err
	}
	if c.options.MaxOutputBytes > 0 {
		size, err := directorySize(stagingDir)
		if err != nil {
			return nil, err
		}
		if size > c.options.MaxOutputBytes {
			return nil, newConversionError(ErrQuotaExceeded, "disk limit", fmt.Errorf("conversion produced %d bytes, limit is %d", size, c.options.MaxOutputBytes))
		}
	}
	return &Result{
		PageCount:    manifest.PageCount,
		Width:        manifest.Width,
		Height:       manifest.Height,
		Pages:        pages,
		Warnings:     warnings,
		PDFPath:      pdfPath,
		ManifestPath: filepath.Join(stagingDir, "manifest.json"),
		StagingDir:   stagingDir,
	}, nil
}

const mathMaxInt64 = int64(^uint64(0) >> 1)

// estimateDocumentPixels returns the total pixel count for rendering every
// page at dpi, enforcing the per-page limit along the way.
func (c *Converter) estimateDocumentPixels(pages []pdfPageGeometry, dpi int) (int64, error) {
	var total int64
	for i, geometry := range pages {
		pixels := geometry.estimatedPixels(dpi)
		if c.options.MaxPixelsPerPage > 0 && pixels > c.options.MaxPixelsPerPage {
			return 0, pageConversionError(ErrQuotaExceeded, "pixel limit", i+1, fmt.Errorf("estimated %d pixels at %d DPI, limit is %d", pixels, dpi, c.options.MaxPixelsPerPage))
		}
		if mathMaxInt64-total < pixels {
			return 0, newConversionError(ErrQuotaExceeded, "pixel limit", errors.New("total pixel count overflows int64"))
		}
		total += pixels
	}
	return total, nil
}

// fitRenderDPI picks the page render DPI. Long documents whose pages would
// exceed MaxTotalPixels at the configured DPI are rendered at a proportionally
// lower DPI (never below MinConversionDPI) instead of being rejected.
func (c *Converter) fitRenderDPI(pages []pdfPageGeometry) (int, *Warning, error) {
	dpi := c.options.ConversionDPI
	total, err := c.estimateDocumentPixels(pages, dpi)
	if err != nil {
		return 0, nil, err
	}
	limit := c.options.MaxTotalPixels
	if limit <= 0 || total <= limit {
		return dpi, nil, nil
	}
	requested := dpi
	floor := c.options.MinConversionDPI
	if floor > requested {
		floor = requested
	}
	// Pixel count scales with DPI squared; step down until the estimate
	// (which rounds each page dimension up) fits.
	dpi = int(math.Floor(float64(dpi) * math.Sqrt(float64(limit)/float64(total))))
	for {
		if dpi < floor {
			minTotal, minErr := c.estimateDocumentPixels(pages, floor)
			if minErr != nil {
				return 0, nil, minErr
			}
			return 0, nil, newConversionError(ErrQuotaExceeded, "pixel limit", fmt.Errorf("estimated %d total pixels at the minimum %d DPI (%d at %d DPI), limit is %d", minTotal, floor, total, requested, limit))
		}
		fitted, fitErr := c.estimateDocumentPixels(pages, dpi)
		if fitErr != nil {
			return 0, nil, fitErr
		}
		if fitted <= limit {
			break
		}
		dpi--
	}
	return dpi, &Warning{
		Code:    "conversion_dpi_reduced",
		Message: fmt.Sprintf("Pages rendered at %d DPI instead of %d DPI to stay within the %d total pixel limit for %d pages.", dpi, requested, limit, len(pages)),
	}, nil
}

func prepareStaging(stagingDir, pagesBase, thumbsBase string) error {
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		return newConversionError(ErrInvalidStaging, "create", err)
	}
	for _, name := range []string{pagesBase, thumbsBase, "renditions", "converted.pdf", "text.json", "manifest.json"} {
		path := filepath.Join(stagingDir, name)
		if _, err := os.Lstat(path); err == nil {
			return newConversionError(ErrInvalidStaging, "prepare", fmt.Errorf("%s already exists", name))
		} else if !errors.Is(err, os.ErrNotExist) {
			return newConversionError(ErrInvalidStaging, "prepare", err)
		}
	}
	for _, name := range []string{pagesBase, thumbsBase, "renditions"} {
		if err := os.Mkdir(filepath.Join(stagingDir, name), 0755); err != nil {
			return newConversionError(ErrInvalidStaging, "prepare", err)
		}
	}
	return nil
}

func samePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	return filepath.Clean(absA) == filepath.Clean(absB)
}

func promoteLegacyAssets(stagingDir, pagesDir, thumbsDir, originalName string) error {
	outDir := filepath.Dir(pagesDir)
	targets := map[string]string{
		filepath.Base(pagesDir):  pagesDir,
		filepath.Base(thumbsDir): thumbsDir,
		"renditions":             filepath.Join(outDir, "renditions"),
		"text.json":              filepath.Join(outDir, "text.json"),
		"manifest.json":          filepath.Join(outDir, "manifest.json"),
		"converted.pdf":          filepath.Join(outDir, "converted.pdf"),
		originalName:             filepath.Join(outDir, originalName),
	}
	backupDir := filepath.Join(stagingDir, ".previous")
	if err := os.Mkdir(backupDir, 0755); err != nil {
		return err
	}
	type replacement struct {
		name        string
		target      string
		hadPrevious bool
		promoted    bool
	}
	order := []string{filepath.Base(pagesDir), filepath.Base(thumbsDir), "renditions", "text.json", "manifest.json", "converted.pdf", originalName}
	replacements := make([]replacement, 0, len(order))
	rollback := func() {
		for i := len(replacements) - 1; i >= 0; i-- {
			item := replacements[i]
			if item.promoted {
				_ = os.RemoveAll(item.target)
			}
			if item.hadPrevious {
				_ = os.Rename(filepath.Join(backupDir, item.name), item.target)
			}
		}
	}
	for _, name := range order {
		target := targets[name]
		item := replacement{name: name, target: target}
		if _, err := os.Lstat(target); err == nil {
			item.hadPrevious = true
			if err := os.Rename(target, filepath.Join(backupDir, name)); err != nil {
				rollback()
				return fmt.Errorf("backup existing %s: %w", name, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			rollback()
			return err
		}
		if err := os.Rename(filepath.Join(stagingDir, name), target); err != nil {
			replacements = append(replacements, item)
			rollback()
			return fmt.Errorf("promote %s: %w", name, err)
		}
		item.promoted = true
		replacements = append(replacements, item)
	}
	return nil
}
