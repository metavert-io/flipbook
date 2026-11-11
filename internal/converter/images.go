package converter

import (
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"sync"
)

func detectDimensions(imagePath string) (int, int, error) {
	return imageDimensions(imagePath)
}

func makeRendition(root, role, format, name string, quality int) (Rendition, error) {
	path, err := artifactPath(root, name)
	if err != nil {
		return Rendition{}, err
	}
	width, height, err := imageDimensions(path)
	if err != nil {
		return Rendition{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Rendition{}, err
	}
	digest, err := fileSHA256(path)
	if err != nil {
		return Rendition{}, err
	}
	return Rendition{
		Role:    role,
		Format:  format,
		Path:    filepath.ToSlash(name),
		Width:   width,
		Height:  height,
		Bytes:   info.Size(),
		SHA256:  digest,
		Quality: quality,
	}, nil
}

func writeJPEG(srcPath, dstPath string, quality int) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	decoded, _, err := image.Decode(src)
	closeErr := src.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}

	out, err := os.CreateTemp(filepath.Dir(dstPath), ".jpeg-*")
	if err != nil {
		return err
	}
	tmp := out.Name()
	defer os.Remove(tmp)
	if err := jpeg.Encode(out, decoded, &jpeg.Options{Quality: quality}); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0644); err != nil {
		return err
	}
	if err := replaceFile(tmp, dstPath); err != nil {
		return fmt.Errorf("install JPEG: %w", err)
	}
	return nil
}

func directorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// outputBudget is shared by concurrently rendering pages.
type outputBudget struct {
	mu    sync.Mutex
	limit int64
	used  int64
}

func (b *outputBudget) add(path string) error {
	if b == nil {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	size := info.Size()
	b.mu.Lock()
	defer b.mu.Unlock()
	if size > mathMaxInt64-b.used {
		return newConversionError(ErrQuotaExceeded, "disk limit", fmt.Errorf("output size overflows int64"))
	}
	if b.limit > 0 && b.used+size > b.limit {
		return newConversionError(ErrQuotaExceeded, "disk limit", fmt.Errorf("conversion output exceeds %d bytes", b.limit))
	}
	b.used += size
	return nil
}
