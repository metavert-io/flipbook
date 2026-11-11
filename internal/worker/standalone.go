package worker

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/metavert-io/flipbook/internal/config"
	"github.com/metavert-io/flipbook/internal/converter"
	"github.com/metavert-io/flipbook/internal/database"
	"github.com/metavert-io/flipbook/internal/storage"
)

func NewWithConfig(db *database.DB, store *storage.Storage, conv *converter.Converter, cfg *config.Config) (*Worker, error) {
	if cfg == nil {
		return nil, errors.New("worker configuration is required")
	}
	return NewWithOptions(db, store, conv, OptionsFromConfig(cfg.Conversion, cfg.Webhooks))
}

// ConverterOptions maps the conversion configuration onto converter options.
// The server's in-process worker and the standalone worker both use it so a
// setting means the same thing whichever process runs the conversion.
func ConverterOptions(cfg *config.Config) converter.Options {
	options := converter.DefaultOptions()
	options.ConversionDPI = cfg.ConversionDPI
	options.ThumbnailDPI = cfg.ThumbnailDPI
	options.Timeout = time.Duration(cfg.Conversion.TimeoutSeconds) * time.Second
	options.PageTimeout = time.Duration(cfg.Conversion.PageTimeoutSeconds) * time.Second
	options.RenderConcurrency = cfg.Conversion.RenderConcurrency
	if options.RenderConcurrency < 0 || options.RenderConcurrency > 64 {
		// config.Validate rejects this; the server loads config leniently, so
		// fall back to automatic rather than fail every conversion.
		options.RenderConcurrency = 0
	}
	options.MaxPages = cfg.Conversion.MaxPages
	options.MaxPixelsPerPage = cfg.Conversion.MaxPagePixels
	options.MaxTotalPixels = cfg.Conversion.MaxTotalPixels
	options.MinConversionDPI = cfg.Conversion.MinDPI
	options.MaxOutputBytes = cfg.Conversion.MaxDiskBytes
	options.OCR.Disabled = !cfg.OCR.Enabled
	options.OCR.Binary = cfg.OCR.TesseractBin
	options.OCR.Language = cfg.OCR.Language
	options.OCR.Timeout = time.Duration(cfg.OCR.TimeoutSeconds) * time.Second
	return options
}

func RunStandalone(ctx context.Context, cfg *config.Config) error {
	if cfg == nil {
		return errors.New("worker configuration is required")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	db, err := database.Open(ctx, cfg.MongoURI, cfg.MongoDB)
	if err != nil {
		return err
	}
	defer db.Close(context.Background())

	storageOptions := storage.Options{CDNBaseURL: cfg.Storage.S3.CDNBaseURL}
	if cfg.Storage.Backend == "s3" {
		objectStore, err := storage.NewS3ObjectStore(storage.S3Config{
			Endpoint: cfg.Storage.S3.Endpoint, Region: cfg.Storage.S3.Region, Bucket: cfg.Storage.S3.Bucket,
			AccessKey: cfg.Storage.S3.AccessKeyID, SecretKey: cfg.Storage.S3.SecretAccessKey,
			SessionToken: cfg.Storage.S3.SessionToken, UseVirtualHost: !cfg.Storage.S3.PathStyle,
		})
		if err != nil {
			return err
		}
		storageOptions.ObjectStore = objectStore
	}
	store := storage.NewWithOptions(filepath.Join(cfg.DataDir, "flipbooks"), storageOptions)

	conv := converter.NewWithOptions(cfg.LibreOfficeBin, filepath.Join(cfg.DataDir, "tmp"), ConverterOptions(cfg))

	worker, err := NewWithOptions(db, store, conv, OptionsFromConfig(cfg.Conversion, cfg.Webhooks))
	if err != nil {
		return err
	}
	return worker.Run(ctx)
}
