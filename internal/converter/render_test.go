package converter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func writeTestPDF(t *testing.T) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), "source.pdf")
	if err := os.WriteFile(source, generatedTestPDF(), 0644); err != nil {
		t.Fatal(err)
	}
	return source
}

func fastTestOptions() Options {
	options := DefaultOptions()
	options.ConversionDPI = 72
	options.ThumbnailDPI = 36
	options.OCR.Disabled = true
	return options
}

// writeTool installs an executable shell script used in place of a Poppler
// binary.
func writeTool(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tool.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConcurrentRenderingMatchesSerialAndReportsProgress(t *testing.T) {
	requirePoppler(t)
	source := writeTestPDF(t)

	convert := func(concurrency int) (*Result, []Progress) {
		options := fastTestOptions()
		options.RenderConcurrency = concurrency
		var mu sync.Mutex
		var reports []Progress
		ctx := WithProgress(context.Background(), func(p Progress) {
			mu.Lock()
			reports = append(reports, p)
			mu.Unlock()
		})
		result, err := NewWithOptions("", "", options).ConvertStaged(ctx, source, filepath.Join(t.TempDir(), "staging"))
		if err != nil {
			t.Fatalf("concurrency %d: %v", concurrency, err)
		}
		return result, reports
	}

	serial, _ := convert(1)
	concurrent, reports := convert(2)
	if !reflect.DeepEqual(serial.Pages, concurrent.Pages) {
		t.Fatalf("concurrent pages differ from serial pages:\nserial: %+v\nconcurrent: %+v", serial.Pages, concurrent.Pages)
	}

	var rendered []int
	sawText := false
	for _, report := range reports {
		switch report.Stage {
		case ProgressRenderPages:
			if sawText {
				t.Fatal("render progress arrived after text extraction started")
			}
			if report.Total != 2 {
				t.Fatalf("render progress total = %d, want 2", report.Total)
			}
			rendered = append(rendered, report.Done)
		case ProgressExtractText:
			sawText = true
		}
	}
	if !reflect.DeepEqual(rendered, []int{0, 1, 2}) {
		t.Fatalf("render progress = %v, want [0 1 2]", rendered)
	}
	if !sawText {
		t.Fatal("text extraction progress was not reported")
	}
}

func TestPageFailureStopsRenderingWithPageError(t *testing.T) {
	requirePoppler(t)
	options := fastTestOptions()
	options.RenderConcurrency = 2
	// -singlefile -f N ...: fail page 2, render everything else normally.
	options.Tools.PDFToPPM = writeTool(t, `if [ "$3" = "2" ]; then echo "Syntax Error: broken page" >&2; exit 1; fi
exec pdftoppm "$@"`)
	_, err := NewWithOptions("", "", options).ConvertStaged(context.Background(), writeTestPDF(t), filepath.Join(t.TempDir(), "staging"))
	if !errors.Is(err, ErrCorruptPDF) {
		t.Fatalf("page failure returned %v, want ErrCorruptPDF", err)
	}
	var conversionErr *ConversionError
	if !errors.As(err, &conversionErr) || conversionErr.Page != 2 {
		t.Fatalf("page failure did not identify page 2: %v", err)
	}
}

func TestTimeLimitFailureIsReportedAsTimeout(t *testing.T) {
	requirePoppler(t)
	options := fastTestOptions()
	options.Timeout = 300 * time.Millisecond
	options.PageTimeout = -1
	options.Tools.PDFToPPM = writeTool(t, "exec sleep 30")
	start := time.Now()
	_, err := NewWithOptions("", "", options).ConvertStaged(context.Background(), writeTestPDF(t), filepath.Join(t.TempDir(), "staging"))
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("time limit returned %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("time limit took %s to stop rendering", elapsed)
	}
}

func TestCallerCancellationIsNotATimeout(t *testing.T) {
	requirePoppler(t)
	options := fastTestOptions()
	options.Timeout = time.Minute
	options.Tools.PDFToPPM = writeTool(t, "exec sleep 30")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	_, err := NewWithOptions("", "", options).ConvertStaged(ctx, writeTestPDF(t), filepath.Join(t.TempDir(), "staging"))
	if err == nil || errors.Is(err, ErrTimeout) {
		t.Fatalf("caller cancellation returned %v, want a non-timeout error", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation returned %v, want context.Canceled", err)
	}
}

func TestTimeLimitGrowsWithPageCount(t *testing.T) {
	limit := newTimeLimit(context.Background(), 10*time.Minute)
	defer limit.stop()
	base, ok := limit.ctx.Deadline()
	if !ok {
		t.Fatal("base time limit has no deadline")
	}
	ctx := limit.extendForPages(60, 30*time.Second)
	extended, ok := ctx.Deadline()
	if !ok {
		t.Fatal("extended time limit has no deadline")
	}
	if got := extended.Sub(base); got != 30*time.Minute {
		t.Fatalf("60 pages extended the limit by %s, want 30m", got)
	}
	if ctx.Err() != nil {
		t.Fatalf("extended context is already done: %v", ctx.Err())
	}

	unlimited := newTimeLimit(context.Background(), 0)
	defer unlimited.stop()
	if _, ok := unlimited.extendForPages(60, 30*time.Second).Deadline(); ok {
		t.Fatal("a disabled base timeout gained a deadline")
	}
}
