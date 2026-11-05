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

	conversionOptions := converter.DefaultOptions()
	conversionOptions.ConversionDPI = cfg.ConversionDPI
	conversionOptions.ThumbnailDPI = cfg.ThumbnailDPI
	conversionOptions.Timeout = time.Duration(cfg.Conversion.TimeoutSeconds) * time.Second
	conversionOptions.MaxPages = cfg.Conversion.MaxPages
	conversionOptions.MaxPixelsPerPage = cfg.Conversion.MaxPagePixels
	conversionOptions.MaxTotalPixels = cfg.Conversion.MaxTotalPixels
	conversionOptions.MinConversionDPI = cfg.Conversion.MinDPI
	conversionOptions.MaxOutputBytes = cfg.Conversion.MaxDiskBytes
	conversionOptions.OCR.Disabled = !cfg.OCR.Enabled
	conversionOptions.OCR.Binary = cfg.OCR.TesseractBin
	conversionOptions.OCR.Language = cfg.OCR.Language
	conversionOptions.OCR.Timeout = time.Duration(cfg.OCR.TimeoutSeconds) * time.Second
	conv := converter.NewWithOptions(cfg.LibreOfficeBin, filepath.Join(cfg.DataDir, "tmp"), conversionOptions)

	worker, err := NewWithOptions(db, store, conv, OptionsFromConfig(cfg.Conversion, cfg.Webhooks))
	if err != nil {
		return err
	}
	return worker.Run(ctx)
}
