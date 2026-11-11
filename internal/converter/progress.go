package converter

import "context"

// ProgressStage names the phase of a conversion a Progress report belongs to.
type ProgressStage string

const (
	// ProgressConvertDocument is the LibreOffice office-to-PDF step.
	ProgressConvertDocument ProgressStage = "convert_document"
	// ProgressRenderPages reports Done of Total pages fully rendered (page
	// image, thumbnail, and JPEG renditions).
	ProgressRenderPages ProgressStage = "render_pages"
	// ProgressExtractText covers text, link, outline extraction and OCR.
	ProgressExtractText ProgressStage = "extract_text"
)

// Progress is one conversion progress report. Done and Total are only set for
// ProgressRenderPages.
type Progress struct {
	Stage ProgressStage
	Done  int
	Total int
}

// ProgressFunc receives conversion progress. Calls are serialized and arrive
// in order, so implementations need no locking of their own; they run on the
// rendering goroutines and should return quickly.
type ProgressFunc func(Progress)

type progressKey struct{}

// WithProgress returns a context that makes Convert and ConvertStaged report
// progress to fn.
func WithProgress(ctx context.Context, fn ProgressFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, progressKey{}, fn)
}

// ReportProgress sends progress to the ProgressFunc attached to ctx with
// WithProgress, if any.
func ReportProgress(ctx context.Context, progress Progress) {
	if fn, ok := ctx.Value(progressKey{}).(ProgressFunc); ok {
		fn(progress)
	}
}
