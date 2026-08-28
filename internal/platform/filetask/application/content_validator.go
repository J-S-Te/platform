package application

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"path"
	"strings"
)

func calculateStoredIntegrity(content io.ReadSeeker) (uint64, []byte, error) {
	if content == nil {
		return 0, nil, errors.New("stored content is required")
	}
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		return 0, nil, fmt.Errorf("rewind stored content: %w", err)
	}
	hash := sha256.New()
	size, err := io.Copy(hash, content)
	if err != nil {
		return 0, nil, fmt.Errorf("hash stored content: %w", err)
	}
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		return 0, nil, fmt.Errorf("rewind stored content after hashing: %w", err)
	}
	return uint64(size), hash.Sum(nil), nil
}

const maxDecodedImagePixels uint64 = 40_000_000

const (
	// ZIP 解压后的总大小、单文件大小和条目数量均受限，避免压缩炸弹耗尽内存或磁盘。
	maxZIPExpandedBytes = 100 << 20
	maxZIPEntryBytes    = 50 << 20
	maxZIPEntries       = 10_000
	maxZIPNestingDepth  = 3
)

// validateStoredContent 对已落入隔离区的文件执行格式级校验。
// MIME 探测只识别文件头，本步骤负责拒绝截断图片、伪造 PDF 和超大图片解码炸弹。
func validateStoredContent(mediaType string, content io.ReadSeeker) error {
	if content == nil {
		return errors.New("stored file is unavailable")
	}
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		return errors.New("stored file cannot be read")
	}
	switch mediaType {
	case "image/jpeg", "image/png":
		configuration, _, err := image.DecodeConfig(content)
		if err != nil || configuration.Width <= 0 || configuration.Height <= 0 {
			return errors.New("image structure is invalid")
		}
		pixels := uint64(configuration.Width) * uint64(configuration.Height)
		if pixels > maxDecodedImagePixels {
			return errors.New("image dimensions exceed the configured limit")
		}
		if _, err := content.Seek(0, io.SeekStart); err != nil {
			return errors.New("stored image cannot be reread")
		}
		if _, _, err := image.Decode(content); err != nil {
			return errors.New("image data is truncated or malformed")
		}
	case "application/pdf":
		if err := validatePDFEnvelope(content); err != nil {
			return err
		}
	case "application/zip", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		if err := validateZIPStructure(mediaType, content); err != nil {
			return err
		}
	}
	return nil
}

// validateZIPStructure 深检 ZIP/OOXML 容器，拒绝路径逃逸、加密条目、过度压缩和结构缺失。
// 这里只读取受限条目，不把整个解压结果写入磁盘；业务层仍应把未知扩展名当作普通 ZIP 处理。
func validateZIPStructure(mediaType string, content io.ReadSeeker) error {
	end, err := content.Seek(0, io.SeekEnd)
	if err != nil || end <= 0 || end > defaultUploadMaxBytes {
		return errors.New("ZIP size is invalid")
	}
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		return errors.New("ZIP cannot be reread")
	}
	data, err := io.ReadAll(io.LimitReader(content, end+1))
	if err != nil || int64(len(data)) != end {
		return errors.New("ZIP content cannot be read")
	}
	budget := &zipValidationBudget{}
	return validateZIPReader(mediaType, data, 0, budget)
}

type zipValidationBudget struct {
	expanded uint64
	entries  int
}

func validateZIPReader(mediaType string, data []byte, depth int, budget *zipValidationBudget) error {
	if depth > maxZIPNestingDepth {
		return errors.New("ZIP nesting depth exceeds the configured limit")
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return errors.New("ZIP structure is invalid")
	}
	if len(reader.File) == 0 || budget.entries+len(reader.File) > maxZIPEntries {
		return errors.New("ZIP entry count is invalid")
	}
	budget.entries += len(reader.File)
	entries := make(map[string]struct{}, len(reader.File))
	for _, entry := range reader.File {
		name := strings.ReplaceAll(entry.Name, "\\", "/")
		clean := path.Clean(name)
		if name == "" || clean == "." || strings.HasPrefix(clean, "../") || clean == ".." || strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) {
			return errors.New("ZIP contains an unsafe entry path")
		}
		if _, duplicate := entries[name]; duplicate {
			return errors.New("ZIP contains duplicate entry names")
		}
		entries[name] = struct{}{}
		if entry.Flags&0x1 != 0 || entry.UncompressedSize64 > maxZIPEntryBytes {
			return errors.New("ZIP contains encrypted or oversized entry")
		}
		if entry.CompressedSize64 == 0 && entry.UncompressedSize64 > 0 || entry.CompressedSize64 > 0 && entry.UncompressedSize64/entry.CompressedSize64 > 1000 {
			return errors.New("ZIP compression ratio is unsafe")
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		stream, err := entry.Open()
		if err != nil {
			return errors.New("ZIP entry cannot be opened")
		}
		entryData, readErr := io.ReadAll(io.LimitReader(stream, maxZIPEntryBytes+1))
		closeErr := stream.Close()
		if readErr != nil || closeErr != nil || uint64(len(entryData)) != entry.UncompressedSize64 || len(entryData) > maxZIPEntryBytes {
			return errors.New("ZIP entry is truncated, oversized or has an invalid checksum")
		}
		budget.expanded += uint64(len(entryData))
		if budget.expanded > maxZIPExpandedBytes {
			return errors.New("ZIP expanded size exceeds the configured limit")
		}
		lowerName := strings.ToLower(name)
		if strings.HasSuffix(lowerName, ".xml") || strings.HasSuffix(lowerName, ".rels") {
			lowerXML := bytes.ToLower(entryData)
			if bytes.Contains(lowerXML, []byte("<!doctype")) || bytes.Contains(lowerXML, []byte("<!entity")) {
				return errors.New("Office XML contains a disallowed DTD or entity declaration")
			}
		}
		if len(entryData) >= 4 && bytes.Equal(entryData[:4], []byte{'P', 'K', 0x03, 0x04}) {
			if err := validateZIPReader("application/zip", entryData, depth+1, budget); err != nil {
				return fmt.Errorf("nested ZIP entry %q is unsafe: %w", name, err)
			}
		}
	}
	if strings.HasPrefix(mediaType, "application/vnd.openxmlformats-officedocument.") {
		// OOXML 必须具备内容类型和关系入口；仅改后缀的任意 ZIP 不能冒充 Office 文件。
		if _, ok := entries["[Content_Types].xml"]; !ok {
			return errors.New("Office package is missing [Content_Types].xml")
		}
		if _, ok := entries["_rels/.rels"]; !ok {
			return errors.New("Office package is missing root relationships")
		}
	}
	return nil
}

// verifyStoredIntegrity 在下载前复核不可变版本的长度与摘要，防止磁盘篡改内容被直接流出。
func verifyStoredIntegrity(content io.ReadSeeker, expectedSize uint64, expectedDigest []byte) error {
	if content == nil || len(expectedDigest) != sha256.Size {
		return errors.New("stored file integrity metadata is incomplete")
	}
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		return errors.New("stored file cannot be read")
	}
	hash := sha256.New()
	size, err := io.Copy(hash, content)
	if err != nil || size < 0 || uint64(size) != expectedSize || subtle.ConstantTimeCompare(hash.Sum(nil), expectedDigest) != 1 {
		return errors.New("stored file integrity verification failed")
	}
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		return errors.New("stored file cannot be rewound")
	}
	return nil
}

func validatePDFEnvelope(content io.ReadSeeker) error {
	header := make([]byte, 8)
	count, err := io.ReadFull(content, header)
	if err != nil || count < 5 || !bytes.HasPrefix(header, []byte("%PDF-")) {
		return errors.New("PDF header is invalid")
	}
	end, err := content.Seek(0, io.SeekEnd)
	if err != nil || end < 8 {
		return errors.New("PDF structure is incomplete")
	}
	tailSize := int64(2048)
	if end < tailSize {
		tailSize = end
	}
	if _, err := content.Seek(-tailSize, io.SeekEnd); err != nil {
		return errors.New("PDF trailer cannot be read")
	}
	tail := make([]byte, tailSize)
	if _, err := io.ReadFull(content, tail); err != nil || !bytes.Contains(tail, []byte("%%EOF")) {
		return errors.New("PDF trailer is invalid")
	}
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		return errors.New("PDF cannot be reread")
	}
	data, err := io.ReadAll(content)
	if err != nil {
		return errors.New("PDF structure cannot be read")
	}
	// 第一阶段按危险字典失败关闭；后续独立网关可替换为 pdfcpu 深度解析器。
	for _, marker := range [][]byte{[]byte("/JavaScript"), []byte("/JS"), []byte("/EmbeddedFile"), []byte("/Launch"), []byte("/Encrypt")} {
		if bytes.Contains(data, marker) {
			return errors.New("PDF contains a disallowed active or embedded feature")
		}
	}
	return nil
}
