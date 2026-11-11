package worker

import (
	"testing"
	"time"

	"github.com/metavert-io/flipbook/internal/config"
)

func TestConverterOptionsApplyConversionConfig(t *testing.T) {
	cfg, err := config.LoadWithError()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Conversion.TimeoutSeconds = 1200
	cfg.Conversion.PageTimeoutSeconds = 45
	cfg.Conversion.RenderConcurrency = 3
	cfg.Conversion.MaxPages = 500
	cfg.Conversion.MaxDiskBytes = 1 << 30

	options := ConverterOptions(cfg)
	if options.Timeout != 20*time.Minute || options.PageTimeout != 45*time.Second {
		t.Fatalf("time limits not applied: timeout=%s page=%s", options.Timeout, options.PageTimeout)
	}
	if options.RenderConcurrency != 3 || options.MaxPages != 500 || options.MaxOutputBytes != 1<<30 {
		t.Fatalf("conversion limits not applied: %+v", options)
	}

	cfg.Conversion.RenderConcurrency = 500
	if got := ConverterOptions(cfg).RenderConcurrency; got != 0 {
		t.Fatalf("out-of-range render concurrency = %d, want automatic (0)", got)
	}
}
