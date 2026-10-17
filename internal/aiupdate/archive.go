package aiupdate

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// matrix/hardened: downloaded packages may only create bounded regular files in
// a fresh staging directory; package completeness is checked before activation.
const (
	maxPackageFileSize = int64(1 << 30)
	maxPackageSize     = int64(2 << 30)
	maxPackageEntries  = 50000
	maxArchivePadding  = int64(1 << 20)
)

type packageExtractLimits struct {
	fileSize  int64
	totalSize int64
	entries   int
}

func extractPackage(archivePath, destination string) error {
	return extractPackageWithLimits(archivePath, destination, packageExtractLimits{
		fileSize: maxPackageFileSize, totalSize: maxPackageSize, entries: maxPackageEntries,
	})
}

func extractPackageWithLimits(archivePath, destination string, limits packageExtractLimits) error {
	info, err := os.Lstat(destination)
	if err != nil {
		return fmt.Errorf("inspect package staging directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("package staging destination must be a directory, not a symlink")
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return fmt.Errorf("open package staging directory: %w", err)
	}
	defer root.Close()
	staging, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("inspect package staging contents: %w", err)
	}
	openedInfo, statErr := staging.Stat()
	entries, readErr := staging.ReadDir(1)
	closeErr := staging.Close()
	if statErr != nil || !os.SameFile(info, openedInfo) {
		return errors.New("package staging directory changed while opening it")
	}
	if len(entries) != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) || closeErr != nil {
		return errors.New("package staging destination must be empty and readable")
	}

	archive, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open package archive: %w", err)
	}
	defer archive.Close()
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("read package gzip header: %w", err)
	}
	defer gz.Close()
	// matrix/hardened: bound metadata and padding as well as file payloads, so
	// gzip bombs hidden outside the logical tar entries cannot run indefinitely.
	stream := &packageArchiveReader{reader: gz, remaining: limits.totalSize + int64(limits.entries)*2048 + maxArchivePadding}
	tr := tar.NewReader(stream)
	seen := make(map[string]byte)
	var total, previousSize, entryBlocks int64
	for {
		before := stream.read
		header, nextErr := tr.Next()
		padding := (512 - previousSize%512) % 512
		if errors.Is(nextErr, io.EOF) {
			// archive/tar accepts a bare EOF without end markers. Require both
			// terminator blocks, then read through the gzip checksum and trailer.
			if stream.read-before != padding+1024 {
				return errors.New("package tar archive is missing its end markers")
			}
			return finishPackageArchive(stream)
		}
		if nextErr != nil {
			return fmt.Errorf("read package tar entry: %w", nextErr)
		}
		// matrix/hardened: Next hides PAX/GNU extension entries. Count their
		// metadata blocks conservatively so they cannot bypass the entry cap.
		entryBlocks += (stream.read - before - padding) / 512
		if entryBlocks > int64(limits.entries) {
			return errors.New("package archive contains too many entries")
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA && header.Typeflag != tar.TypeDir {
			return errors.New("package archive contains a link or unsupported entry")
		}
		for key := range header.PAXRecords {
			if strings.HasPrefix(key, "GNU.sparse.") {
				return errors.New("package archive contains an unsupported sparse file")
			}
		}
		name, err := packageArchiveName(header.Name)
		if err != nil {
			return err
		}
		previousSize = header.Size
		if header.Typeflag == tar.TypeDir {
			if header.Size != 0 {
				return errors.New("package archive directory contains file data")
			}
			if kind, exists := seen[name]; exists && kind != tar.TypeDir {
				return errors.New("package archive reuses a file as a directory")
			}
			seen[name] = tar.TypeDir
			if err := ensurePackageDirectories(root, name); err != nil {
				return err
			}
			continue
		}
		if name == "." {
			return errors.New("package archive root cannot be a file")
		}
		if _, exists := seen[name]; exists {
			return errors.New("package archive contains a duplicate file")
		}
		seen[name] = tar.TypeReg
		if header.Size < 0 || header.Size > limits.fileSize || header.Size > limits.totalSize-total {
			return errors.New("package archive exceeds its extracted size limit")
		}
		total += header.Size
		if err := ensurePackageDirectories(root, path.Dir(name)); err != nil {
			return err
		}
		mode := os.FileMode(0644) | os.FileMode(header.Mode)&0111
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return fmt.Errorf("create package file: %w", err)
		}
		_, copyErr := io.CopyN(file, tr, header.Size)
		chmodErr := file.Chmod(mode)
		closeErr := file.Close()
		if err := errors.Join(copyErr, chmodErr, closeErr); err != nil {
			return fmt.Errorf("extract package file: %w", err)
		}
	}
}

func packageArchiveName(name string) (string, error) {
	// matrix/hardened: reject ambiguous Windows names on every platform, and
	// reject traversal even if path.Clean would happen to bring it back inside.
	if name == "" || strings.ContainsAny(name, "\\:\x00") || path.IsAbs(name) {
		return "", errors.New("package archive contains an unsafe path")
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", errors.New("package archive contains parent-directory traversal")
		}
	}
	return path.Clean(name), nil
}

func ensurePackageDirectories(root *os.Root, name string) error {
	if name == "." {
		return nil
	}
	current := ""
	for _, component := range strings.Split(name, "/") {
		current = path.Join(current, component)
		info, err := root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := root.Mkdir(current, 0755); err != nil {
				return fmt.Errorf("create package directory: %w", err)
			}
			directory, err := root.Open(current)
			if err != nil {
				return fmt.Errorf("open new package directory: %w", err)
			}
			chmodErr := directory.Chmod(0755)
			closeErr := directory.Close()
			if err := errors.Join(chmodErr, closeErr); err != nil {
				return fmt.Errorf("set package directory permissions: %w", err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect package directory: %w", err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("package path crosses a symlink or non-directory")
		}
	}
	return nil
}

type packageArchiveReader struct {
	reader    io.Reader
	remaining int64
	read      int64
}

func (r *packageArchiveReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, errors.New("package archive exceeds its decompressed stream limit")
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	r.read += int64(n)
	return n, err
}

func finishPackageArchive(reader io.Reader) error {
	// matrix/hardened: tar permits zero padding, but never ignore a second
	// payload, a truncated gzip footer, or an unbounded run of compressed zeros.
	buffer := make([]byte, 32*1024)
	var padding int64
	for {
		n, err := reader.Read(buffer)
		padding += int64(n)
		if padding > maxArchivePadding {
			return errors.New("package archive contains excessive trailing padding")
		}
		for _, value := range buffer[:n] {
			if value != 0 {
				return errors.New("package archive contains data after its end markers")
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("verify package gzip trailer: %w", err)
		}
	}
}
