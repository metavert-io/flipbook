package converter

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"
)

// pageRenderJob describes one document's page rendering. pdfPath and
// stagingDir must be absolute; the pages, thumbs, and renditions directories
// must already exist.
type pageRenderJob struct {
	pdfPath    string
	stagingDir string
	pagesBase  string
	thumbsBase string
	dpi        int
	geometry   []pdfPageGeometry
	budget     *outputBudget
}

// renderPages renders every page with up to Options.RenderConcurrency pages in
// flight. The first failure cancels the remaining pages and is the error
// returned. Progress is reported after each completed page.
func (c *Converter) renderPages(ctx context.Context, job pageRenderJob) ([]PageManifest, []Warning, error) {
	total := len(job.geometry)
	workers := c.options.RenderConcurrency
	if workers > total {
		workers = total
	}
	if workers < 1 {
		workers = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	pages := make([]PageManifest, total)
	pageWarnings := make([][]Warning, total)
	indexes := make(chan int)
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		done     int
	)
	ReportProgress(ctx, Progress{Stage: ProgressRenderPages, Done: 0, Total: total})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range indexes {
				page, warnings, err := c.renderPage(ctx, job, index)
				mu.Lock()
				pageWarnings[index] = warnings
				switch {
				case err != nil:
					if firstErr == nil {
						firstErr = err
						cancel()
					}
				case firstErr == nil:
					pages[index] = page
					done++
					ReportProgress(ctx, Progress{Stage: ProgressRenderPages, Done: done, Total: total})
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for index := 0; index < total; index++ {
		select {
		case indexes <- index:
		case <-ctx.Done():
			break feed
		}
	}
	close(indexes)
	wg.Wait()

	var warnings []Warning
	for _, list := range pageWarnings {
		warnings = append(warnings, list...)
	}
	if firstErr != nil {
		return nil, warnings, firstErr
	}
	if done < total {
		// Only cancellation of the parent context stops the feed without a
		// page error.
		if err := ctx.Err(); err != nil {
			return nil, warnings, err
		}
		return nil, warnings, fmt.Errorf("rendered %d of %d pages", done, total)
	}
	return pages, warnings, nil
}

// renderPage produces the page image, thumbnail, both JPEG renditions, and the
// manifest entry for the zero-based page index.
func (c *Converter) renderPage(ctx context.Context, job pageRenderJob, index int) (PageManifest, []Warning, error) {
	pageNumber := index + 1
	pageName := fmt.Sprintf("page-%d.png", pageNumber)
	pagesDir := filepath.Join(job.stagingDir, job.pagesBase)
	thumbsDir := filepath.Join(job.stagingDir, job.thumbsBase)
	renditionsDir := filepath.Join(job.stagingDir, "renditions")

	warnings, err := c.renderPDFPage(ctx, job.pdfPath, pagesDir, pageNumber, job.dpi, job.budget)
	if err != nil {
		return PageManifest{}, warnings, err
	}
	thumbWarnings, err := c.renderPDFPage(ctx, job.pdfPath, thumbsDir, pageNumber, c.options.ThumbnailDPI, job.budget)
	warnings = append(warnings, thumbWarnings...)
	if err != nil {
		return PageManifest{}, warnings, err
	}

	pagePath := filepath.Join(pagesDir, pageName)
	thumbPath := filepath.Join(thumbsDir, pageName)
	pageJPEG := filepath.Join(renditionsDir, fmt.Sprintf("page-%d.jpg", pageNumber))
	thumbJPEG := filepath.Join(renditionsDir, fmt.Sprintf("thumb-%d.jpg", pageNumber))
	if err := writeJPEG(pagePath, pageJPEG, c.options.JPEGQuality); err != nil {
		return PageManifest{}, warnings, pageConversionError(err, "JPEG page rendition", pageNumber, nil)
	}
	if err := job.budget.add(pageJPEG); err != nil {
		return PageManifest{}, warnings, pageConversionError(nil, "disk limit", pageNumber, err)
	}
	if err := writeJPEG(thumbPath, thumbJPEG, c.options.JPEGQuality); err != nil {
		return PageManifest{}, warnings, pageConversionError(err, "JPEG thumbnail rendition", pageNumber, nil)
	}
	if err := job.budget.add(thumbJPEG); err != nil {
		return PageManifest{}, warnings, pageConversionError(nil, "disk limit", pageNumber, err)
	}

	width, height, err := imageDimensions(pagePath)
	if err != nil {
		return PageManifest{}, warnings, pageConversionError(ErrArtifactValidation, "page geometry", pageNumber, err)
	}
	actualPixels := int64(width) * int64(height)
	if c.options.MaxPixelsPerPage > 0 && actualPixels > c.options.MaxPixelsPerPage {
		return PageManifest{}, warnings, pageConversionError(ErrQuotaExceeded, "rendered pixel limit", pageNumber, fmt.Errorf("got %d pixels, limit is %d", actualPixels, c.options.MaxPixelsPerPage))
	}
	pagePNG, err := makeRendition(job.stagingDir, "page", "png", filepath.Join(job.pagesBase, pageName), 0)
	if err != nil {
		return PageManifest{}, warnings, pageConversionError(err, "page rendition", pageNumber, nil)
	}
	pageJPG, err := makeRendition(job.stagingDir, "page", "jpeg", filepath.Join("renditions", fmt.Sprintf("page-%d.jpg", pageNumber)), c.options.JPEGQuality)
	if err != nil {
		return PageManifest{}, warnings, pageConversionError(err, "page rendition", pageNumber, nil)
	}
	thumbPNG, err := makeRendition(job.stagingDir, "thumbnail", "png", filepath.Join(job.thumbsBase, pageName), 0)
	if err != nil {
		return PageManifest{}, warnings, pageConversionError(err, "thumbnail rendition", pageNumber, nil)
	}
	thumbJPG, err := makeRendition(job.stagingDir, "thumbnail", "jpeg", filepath.Join("renditions", fmt.Sprintf("thumb-%d.jpg", pageNumber)), c.options.JPEGQuality)
	if err != nil {
		return PageManifest{}, warnings, pageConversionError(err, "thumbnail rendition", pageNumber, nil)
	}
	widthPoints, heightPoints := job.geometry[index].displayPoints()
	return PageManifest{
		Number:       pageNumber,
		Width:        width,
		Height:       height,
		WidthPoints:  widthPoints,
		HeightPoints: heightPoints,
		Rotation:     job.geometry[index].rotation,
		TextSource:   "none",
		Images:       []Rendition{pagePNG, pageJPG, thumbPNG, thumbJPG},
	}, warnings, nil
}

// timeLimit is a conversion's deadline. It starts at Options.Timeout, measured
// from the start of the conversion, and is raised by PageTimeout per page once
// the page count is known.
type timeLimit struct {
	parent  context.Context
	started time.Time
	budget  time.Duration
	pages   int
	ctx     context.Context
	cancel  context.CancelFunc
}

func newTimeLimit(parent context.Context, budget time.Duration) *timeLimit {
	limit := &timeLimit{parent: parent, started: time.Now(), ctx: parent, cancel: func() {}}
	limit.set(budget)
	return limit
}

// set must only be called while nothing is using the current ctx.
func (l *timeLimit) set(budget time.Duration) {
	if budget <= 0 {
		return
	}
	ctx, cancel := context.WithDeadline(l.parent, l.started.Add(budget))
	l.cancel()
	l.ctx, l.cancel, l.budget = ctx, cancel, budget
}

// extendForPages raises the limit for a document of the given length and
// returns the context to use from now on. A zero base timeout stays unlimited.
func (l *timeLimit) extendForPages(pages int, perPage time.Duration) context.Context {
	l.pages = pages
	if l.budget > 0 && perPage > 0 && pages > 0 {
		l.set(l.budget + time.Duration(pages)*perPage)
	}
	return l.ctx
}

func (l *timeLimit) stop() {
	l.cancel()
}

// classify reports a failure caused by this limit as ErrTimeout so callers can
// tell it apart from shutdown or a caller-imposed deadline.
func (l *timeLimit) classify(err error) error {
	if err == nil || l.budget <= 0 || l.parent.Err() != nil || !errors.Is(l.ctx.Err(), context.DeadlineExceeded) {
		return err
	}
	detail := fmt.Sprintf("stopped after %s", l.budget.Round(time.Second))
	if l.pages > 0 {
		detail += fmt.Sprintf(" on a %d-page document", l.pages)
	}
	return newConversionError(ErrTimeout, "", fmt.Errorf("%s: %w", detail, err))
}
