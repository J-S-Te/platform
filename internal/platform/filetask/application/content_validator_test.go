package application

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func makeZIP(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var encoded bytes.Buffer
	writer := zip.NewWriter(&encoded)
	for name, value := range entries {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func makeZIPBytes(t *testing.T, name string, value []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	writer := zip.NewWriter(&encoded)
	entry, err := writer.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write(value); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestValidateStoredContentAcceptsCompletePNG(t *testing.T) {
	t.Parallel()
	var encoded bytes.Buffer
	picture := image.NewRGBA(image.Rect(0, 0, 2, 2))
	picture.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&encoded, picture); err != nil {
		t.Fatal(err)
	}
	if err := validateStoredContent("image/png", bytes.NewReader(encoded.Bytes())); err != nil {
		t.Fatalf("valid PNG rejected: %v", err)
	}
}

func TestVerifyStoredIntegrityRejectsSizeAndDigestMismatch(t *testing.T) {
	t.Parallel()
	content := []byte("trusted content")
	digest := sha256.Sum256(content)
	if err := verifyStoredIntegrity(bytes.NewReader(content), uint64(len(content)), digest[:]); err != nil {
		t.Fatalf("valid content rejected: %v", err)
	}
	if err := verifyStoredIntegrity(bytes.NewReader(content), uint64(len(content)+1), digest[:]); err == nil {
		t.Fatal("size mismatch accepted")
	}
	wrongDigest := sha256.Sum256([]byte("different"))
	if err := verifyStoredIntegrity(bytes.NewReader(content), uint64(len(content)), wrongDigest[:]); err == nil {
		t.Fatal("digest mismatch accepted")
	}
}

func TestValidateStoredContentRejectsTruncatedImageAndPDF(t *testing.T) {
	t.Parallel()
	if err := validateStoredContent("image/png", bytes.NewReader([]byte("\x89PNG\r\n\x1a\n"))); err == nil {
		t.Fatal("truncated PNG accepted")
	}
	if err := validateStoredContent("application/pdf", bytes.NewReader([]byte("%PDF-1.7\n1 0 obj\n"))); err == nil {
		t.Fatal("PDF without trailer accepted")
	}
	if err := validateStoredContent("application/pdf", bytes.NewReader([]byte("%PDF-1.7\n%%EOF\n"))); err != nil {
		t.Fatalf("valid PDF envelope rejected: %v", err)
	}
	if err := validateStoredContent("application/pdf", bytes.NewReader([]byte("%PDF-1.7\n/JavaScript\n%%EOF\n"))); err == nil {
		t.Fatal("active PDF accepted")
	}
}

func TestValidateStoredContentChecksOfficeContainer(t *testing.T) {
	t.Parallel()
	valid := makeZIP(t, map[string]string{"[Content_Types].xml": "types", "_rels/.rels": "rels", "word/document.xml": "document"})
	if err := validateStoredContent("application/vnd.openxmlformats-officedocument.wordprocessingml.document", bytes.NewReader(valid)); err != nil {
		t.Fatalf("valid Office package rejected: %v", err)
	}
	missing := makeZIP(t, map[string]string{"word/document.xml": "document"})
	if err := validateStoredContent("application/vnd.openxmlformats-officedocument.wordprocessingml.document", bytes.NewReader(missing)); err == nil {
		t.Fatal("Office package without required relationship files accepted")
	}
	unsafe := makeZIP(t, map[string]string{"../escape.txt": "escape"})
	if err := validateStoredContent("application/zip", bytes.NewReader(unsafe)); err == nil {
		t.Fatal("ZIP path traversal accepted")
	}
	entity := makeZIP(t, map[string]string{"[Content_Types].xml": "<!DOCTYPE x [<!ENTITY bomb 'x'>]>", "_rels/.rels": "rels"})
	if err := validateStoredContent("application/vnd.openxmlformats-officedocument.wordprocessingml.document", bytes.NewReader(entity)); err == nil {
		t.Fatal("Office XML entity declaration accepted")
	}
}

func TestValidateStoredContentRejectsDeeplyNestedZIP(t *testing.T) {
	t.Parallel()
	nested := makeZIPBytes(t, "leaf.txt", []byte("ok"))
	for depth := 0; depth <= maxZIPNestingDepth; depth++ {
		nested = makeZIPBytes(t, "nested.zip", nested)
	}
	if err := validateStoredContent("application/zip", bytes.NewReader(nested)); err == nil {
		t.Fatal("ZIP nesting beyond configured depth accepted")
	}
}
