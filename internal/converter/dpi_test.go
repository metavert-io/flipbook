package converter

import (
	"errors"
	"testing"
)

func a4Pages(n int) []pdfPageGeometry {
	pages := make([]pdfPageGeometry, n)
	for i := range pages {
		pages[i] = pdfPageGeometry{widthPoints: 595.28, heightPoints: 841.89}
	}
	return pages
}

func TestFitRenderDPIKeepsConfiguredDPIWhenWithinLimit(t *testing.T) {
	c := NewWithOptions("", "", DefaultOptions())
	dpi, warning, err := c.fitRenderDPI(a4Pages(100))
	if err != nil || warning != nil || dpi != defaultConversionDPI {
		t.Fatalf("got dpi=%d warning=%v err=%v", dpi, warning, err)
	}
}

func TestFitRenderDPIReducesDPIForLongDocuments(t *testing.T) {
	c := NewWithOptions("", "", DefaultOptions())
	pages := a4Pages(540) // ~4.7 billion pixels at 300 DPI
	dpi, warning, err := c.fitRenderDPI(pages)
	if err != nil {
		t.Fatal(err)
	}
	if warning == nil || warning.Code != "conversion_dpi_reduced" {
		t.Fatalf("expected DPI reduction warning, got %v", warning)
	}
	if dpi >= defaultConversionDPI || dpi < defaultMinConversionDPI {
		t.Fatalf("unexpected dpi %d", dpi)
	}
	total, err := c.estimateDocumentPixels(pages, dpi)
	if err != nil || total > c.options.MaxTotalPixels {
		t.Fatalf("dpi %d gives %d pixels (err %v), limit %d", dpi, total, err, c.options.MaxTotalPixels)
	}
	above, _ := c.estimateDocumentPixels(pages, dpi+1)
	if above <= c.options.MaxTotalPixels {
		t.Fatalf("dpi %d is not the largest fitting DPI", dpi)
	}
}

func TestFitRenderDPIRejectsWhenMinimumDPIStillTooLarge(t *testing.T) {
	opts := DefaultOptions()
	opts.MaxTotalPixels = 1_000_000
	c := NewWithOptions("", "", opts)
	_, _, err := c.fitRenderDPI(a4Pages(50))
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected quota error, got %v", err)
	}
}
