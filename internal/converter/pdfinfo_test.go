package converter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Poppler appends "(A4)", "(letter)" etc. only for standard paper sizes, so
// custom trim sizes produce one fewer field on the size lines.
const pdfInfoSummaryCustomSize = `Producer:        InDesign
Tagged:          no
Pages:           3
Encrypted:       no
Page size:       1000 x 700 pts
Page rot:        0
File size:       330 bytes
PDF version:     1.4
`

const pdfInfoDetailedCustomSize = `Pages:           3
Encrypted:       no
Page    1 size:  1000 x 700 pts
Page    1 rot:   0
Page    2 size:  595.276 x 841.89 pts (A4)
Page    2 rot:   90
Page    3 size:  432 x 648 pts
Page    3 rot:   0
`

func TestParsePDFInfoAcceptsSummarySizeWithoutPaperName(t *testing.T) {
	var info pdfDocumentInfo
	if err := parsePDFInfo([]byte(pdfInfoSummaryCustomSize), &info); err != nil {
		t.Fatalf("parsePDFInfo: %v", err)
	}
	if info.pageCount != 3 || len(info.pages) != 3 {
		t.Fatalf("got pageCount=%d pages=%d", info.pageCount, len(info.pages))
	}
	for i, page := range info.pages {
		if page.widthPoints != 1000 || page.heightPoints != 700 {
			t.Errorf("page %d: got %+v", i+1, page)
		}
	}
}

func TestParsePDFInfoMergesPerPageGeometry(t *testing.T) {
	var info pdfDocumentInfo
	if err := parsePDFInfo([]byte(pdfInfoSummaryCustomSize), &info); err != nil {
		t.Fatal(err)
	}
	if err := parsePDFInfo([]byte(pdfInfoDetailedCustomSize), &info); err != nil {
		t.Fatal(err)
	}
	want := []pdfPageGeometry{
		{widthPoints: 1000, heightPoints: 700},
		{widthPoints: 595.276, heightPoints: 841.89, rotation: 90},
		{widthPoints: 432, heightPoints: 648},
	}
	for i := range want {
		if info.pages[i] != want[i] {
			t.Errorf("page %d: got %+v want %+v", i+1, info.pages[i], want[i])
		}
	}
}

func TestParsePDFInfoSummaryRotationIsKept(t *testing.T) {
	var info pdfDocumentInfo
	output := "Pages:           1\nPage size:       612 x 792 pts (letter)\nPage rot:        270\n"
	if err := parsePDFInfo([]byte(output), &info); err != nil {
		t.Fatal(err)
	}
	if got := info.pages[0]; got.rotation != 270 {
		t.Fatalf("got %+v", got)
	}
}

func TestParsePDFInfoToleratesTruncatedLines(t *testing.T) {
	var info pdfDocumentInfo
	output := "Pages: 2\nPage rot:\nPage size:\nPage size: 612 x\nPage 1 rot:\nPage 1\nPage 1 size: 612 x 792 pts\n"
	if err := parsePDFInfo([]byte(output), &info); err != nil {
		t.Fatalf("parsePDFInfo: %v", err)
	}
	if info.pages[0].widthPoints != 612 || info.pages[1].widthPoints != 0 {
		t.Fatalf("got %+v", info.pages)
	}
}

func TestParsePDFInfoRejectsInvalidPerPageGeometry(t *testing.T) {
	for _, line := range []string{
		"Page    1 size:  0 x 792 pts",
		"Page    1 size:  612 x -1 pts",
		"Page    1 size:  abc x 792 pts",
		"Page    1 size:  Inf x 792 pts",
	} {
		var info pdfDocumentInfo
		if err := parsePDFInfo([]byte("Pages: 1\n"+line+"\n"), &info); err == nil {
			t.Errorf("%q: expected error", line)
		}
	}
}

func TestInspectPDFCustomPageSizeWithPoppler(t *testing.T) {
	requirePoppler(t)
	path := filepath.Join(t.TempDir(), "custom.pdf")
	if err := os.WriteFile(path, minimalPDF([][2]float64{{1000, 700}, {432, 648}}), 0644); err != nil {
		t.Fatal(err)
	}
	c := NewWithOptions("", "", DefaultOptions())
	info, _, err := c.inspectPDF(context.Background(), path)
	if err != nil {
		t.Fatalf("inspectPDF: %v", err)
	}
	if info.pageCount != 2 {
		t.Fatalf("got %d pages", info.pageCount)
	}
	if got := info.pages[0]; got.widthPoints != 1000 || got.heightPoints != 700 {
		t.Errorf("page 1: %+v", got)
	}
	if got := info.pages[1]; got.widthPoints != 432 || got.heightPoints != 648 {
		t.Errorf("page 2: %+v", got)
	}
}

func minimalPDF(sizes [][2]float64) []byte {
	kids := make([]string, len(sizes))
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", ""}
	for i, size := range sizes {
		kids[i] = fmt.Sprintf("%d 0 R", i+3)
		objects = append(objects, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %g %g] >>", size[0], size[1]))
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(sizes))
	var out strings.Builder
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, object := range objects {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return []byte(out.String())
}
