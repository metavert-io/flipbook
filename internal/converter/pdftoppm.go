package converter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type pdfPageGeometry struct {
	widthPoints  float64
	heightPoints float64
	rotation     int
}

type pdfDocumentInfo struct {
	pageCount int
	encrypted bool
	pages     []pdfPageGeometry
}

func (c *Converter) pdfToPNG(ctx context.Context, pdfPath, outDir string, dpi int) (int, error) {
	info, _, err := c.inspectPDF(ctx, pdfPath)
	if err != nil {
		return 0, err
	}
	_, err = c.renderPDFPages(ctx, pdfPath, outDir, dpi, info.pageCount, nil)
	return info.pageCount, err
}

// renderPDFPages renders every page of pdfPath into outDir, one page at a
// time. ConvertStaged uses renderPDFPage directly so pages can be rendered
// concurrently.
func (c *Converter) renderPDFPages(ctx context.Context, pdfPath, outDir string, dpi, pageCount int, budget *outputBudget) ([]Warning, error) {
	absOut, err := filepath.Abs(outDir)
	if err != nil {
		return nil, err
	}
	absPDF, err := filepath.Abs(pdfPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absOut, 0755); err != nil {
		return nil, fmt.Errorf("create output directory: %w", err)
	}
	var warnings []Warning
	for page := 1; page <= pageCount; page++ {
		pageWarnings, err := c.renderPDFPage(ctx, absPDF, absOut, page, dpi, budget)
		warnings = append(warnings, pageWarnings...)
		if err != nil {
			return warnings, err
		}
	}
	return warnings, nil
}

// renderPDFPage renders a single page to outDir/page-N.png. Rendering one
// page per pdftoppm invocation keeps memory bounded and preserves the
// page-N.png names expected by legacy viewers. absPDF and outDir must be
// absolute and outDir must exist.
func (c *Converter) renderPDFPage(ctx context.Context, absPDF, outDir string, page, dpi int, budget *outputBudget) ([]Warning, error) {
	outputPrefix := filepath.Join(outDir, fmt.Sprintf("page-%d", page))
	result, err := c.runCommand(ctx, c.options.Tools.PDFToPPM,
		"-singlefile",
		"-f", strconv.Itoa(page),
		"-l", strconv.Itoa(page),
		"-png",
		"-rx", strconv.Itoa(dpi),
		"-ry", strconv.Itoa(dpi),
		absPDF,
		outputPrefix,
	)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || ctx.Err() != nil {
			return nil, pageConversionError(nil, "pdftoppm", page, commandError(c.options.Tools.PDFToPPM, err, result))
		}
		return nil, pageConversionError(ErrCorruptPDF, "pdftoppm", page, commandError(c.options.Tools.PDFToPPM, err, result))
	}
	warnings := warningsFromCommand("pdftoppm", result.stderr, page)
	outputPath := outputPrefix + ".png"
	if info, err := os.Stat(outputPath); err != nil || info.Size() == 0 {
		if err == nil {
			err = errors.New("empty output file")
		}
		return warnings, pageConversionError(ErrArtifactValidation, "pdftoppm produced no output", page, err)
	}
	if err := budget.add(outputPath); err != nil {
		return warnings, pageConversionError(nil, "disk limit", page, err)
	}
	return warnings, nil
}

func (c *Converter) inspectPDF(ctx context.Context, pdfPath string) (pdfDocumentInfo, []Warning, error) {
	var info pdfDocumentInfo
	result, err := c.runCommand(ctx, c.options.Tools.PDFInfo, pdfPath)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || ctx.Err() != nil {
			return info, nil, commandError(c.options.Tools.PDFInfo, err, result)
		}
		return info, nil, classifyPDFToolError("pdfinfo", err, result.stderr)
	}
	warnings := warningsFromCommand("pdfinfo", result.stderr, 0)
	if err := parsePDFInfo(result.stdout, &info); err != nil {
		return info, warnings, newConversionError(ErrCorruptPDF, "pdfinfo", err)
	}
	if info.pageCount < 1 {
		return info, warnings, newConversionError(ErrCorruptPDF, "pdfinfo", errors.New("PDF has no pages"))
	}

	detailed, err := c.runCommand(ctx, c.options.Tools.PDFInfo,
		"-f", "1", "-l", strconv.Itoa(info.pageCount), pdfPath)
	if err == nil {
		warnings = append(warnings, warningsFromCommand("pdfinfo", detailed.stderr, 0)...)
		if err := parsePDFInfo(detailed.stdout, &info); err != nil {
			return info, warnings, newConversionError(ErrCorruptPDF, "pdfinfo page geometry", err)
		}
	} else {
		warnings = append(warnings, Warning{Code: "page_geometry_unavailable", Message: commandError(c.options.Tools.PDFInfo, err, detailed).Error()})
	}
	if len(info.pages) == 0 {
		return info, warnings, newConversionError(ErrCorruptPDF, "pdfinfo", errors.New("PDF page geometry is unavailable"))
	}
	for len(info.pages) < info.pageCount {
		info.pages = append(info.pages, info.pages[len(info.pages)-1])
	}
	for i, page := range info.pages {
		if page.widthPoints <= 0 || page.heightPoints <= 0 {
			return info, warnings, newConversionError(ErrCorruptPDF, "pdfinfo", fmt.Errorf("missing geometry for page %d", i+1))
		}
	}
	return info, warnings, nil
}

// parsePDFInfo merges one pdfinfo report into info. It is called twice: once
// for the summary report (page count plus page 1's size as "Page size:") and
// once with -f/-l for per-page "Page N size:" lines. Geometry completeness is
// checked by the caller after both passes, so a summary report alone never
// fails the document.
func parsePDFInfo(output []byte, info *pdfDocumentInfo) error {
	var defaultGeometry *pdfPageGeometry
	if len(info.pages) == 0 && info.pageCount > 0 {
		info.pages = make([]pdfPageGeometry, info.pageCount)
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "Pages:":
			pages, err := strconv.Atoi(fields[1])
			if err != nil || pages < 1 {
				return errors.New("pdfinfo did not report a valid page count")
			}
			info.pageCount = pages
			if len(info.pages) == 0 {
				info.pages = make([]pdfPageGeometry, pages)
			}
		case "Encrypted:":
			info.encrypted = strings.EqualFold(fields[1], "yes")
		case "Page":
			switch fields[1] {
			case "size:":
				if width, height, ok := parsePDFPageSize(fields[2:]); ok {
					if defaultGeometry == nil {
						defaultGeometry = &pdfPageGeometry{}
					}
					defaultGeometry.widthPoints = width
					defaultGeometry.heightPoints = height
				}
				continue
			case "rot:":
				if len(fields) < 3 {
					continue
				}
				if rotation, err := strconv.Atoi(fields[2]); err == nil {
					if defaultGeometry == nil {
						defaultGeometry = &pdfPageGeometry{}
					}
					defaultGeometry.rotation = normalizeRotation(rotation)
				}
				continue
			}
			if len(fields) < 4 {
				continue
			}
			page, err := strconv.Atoi(fields[1])
			if err != nil || page < 1 {
				continue
			}
			for len(info.pages) < page {
				info.pages = append(info.pages, pdfPageGeometry{})
			}
			switch fields[2] {
			case "size:":
				width, height, ok := parsePDFPageSize(fields[3:])
				if !ok {
					return fmt.Errorf("invalid geometry for page %d", page)
				}
				info.pages[page-1].widthPoints = width
				info.pages[page-1].heightPoints = height
			case "rot:":
				if rotation, err := strconv.Atoi(fields[3]); err == nil {
					info.pages[page-1].rotation = normalizeRotation(rotation)
				}
			}
		}
	}
	if info.pageCount > 0 && len(info.pages) == 0 {
		info.pages = make([]pdfPageGeometry, info.pageCount)
	}
	if defaultGeometry != nil && defaultGeometry.widthPoints > 0 {
		for i := range info.pages {
			if info.pages[i].widthPoints == 0 {
				info.pages[i] = *defaultGeometry
			}
		}
	}
	return nil
}

// parsePDFPageSize reads "W x H pts [(paper name)]". Poppler only appends the
// paper name when the size matches a standard format, so custom trim sizes
// arrive without it and must not be required.
func parsePDFPageSize(fields []string) (float64, float64, bool) {
	if len(fields) < 3 || fields[1] != "x" {
		return 0, 0, false
	}
	width, errWidth := strconv.ParseFloat(fields[0], 64)
	height, errHeight := strconv.ParseFloat(fields[2], 64)
	if errWidth != nil || errHeight != nil {
		return 0, 0, false
	}
	if !(width > 0) || !(height > 0) || math.IsInf(width, 0) || math.IsInf(height, 0) {
		return 0, 0, false
	}
	return width, height, true
}

func normalizeRotation(rotation int) int {
	return ((rotation % 360) + 360) % 360
}

func (g pdfPageGeometry) displayPoints() (float64, float64) {
	if g.rotation%180 == 90 {
		return g.heightPoints, g.widthPoints
	}
	return g.widthPoints, g.heightPoints
}

func (g pdfPageGeometry) estimatedPixels(dpi int) int64 {
	width, height := g.displayPoints()
	pixels := math.Ceil(width*float64(dpi)/72) * math.Ceil(height*float64(dpi)/72)
	if pixels >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(pixels)
}

func pdfPageCount(ctx context.Context, pdfPath string) (int, error) {
	c := NewWithOptions("", "", DefaultOptions())
	info, _, err := c.inspectPDF(ctx, pdfPath)
	if err != nil {
		return 0, err
	}
	return info.pageCount, nil
}
