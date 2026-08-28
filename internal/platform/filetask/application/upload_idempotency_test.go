package application

import (
	"bytes"
	"testing"
)

func TestUploadRequestHashCoversEverySemanticUploadField(t *testing.T) {
	base := UploadInput{
		TenantID: "tenant", ApplicationID: "application", OwnerUserID: "owner",
		RequestID: "request", OriginalName: "report.pdf", Classification: "INTERNAL",
	}
	content := []byte("content")
	want := uploadRequestHash(base, "application/pdf", content)
	cases := []struct {
		name      string
		input     UploadInput
		mediaType string
		content   []byte
	}{
		{name: "tenant", input: withUploadField(base, "tenant"), mediaType: "application/pdf", content: content},
		{name: "application", input: withUploadField(base, "application"), mediaType: "application/pdf", content: content},
		{name: "owner", input: withUploadField(base, "owner"), mediaType: "application/pdf", content: content},
		{name: "request", input: withUploadField(base, "request"), mediaType: "application/pdf", content: content},
		{name: "name", input: withUploadField(base, "name"), mediaType: "application/pdf", content: content},
		{name: "classification", input: withUploadField(base, "classification"), mediaType: "application/pdf", content: content},
		{name: "media", input: base, mediaType: "text/plain", content: content},
		{name: "content", input: base, mediaType: "application/pdf", content: []byte("different")},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := uploadRequestHash(test.input, test.mediaType, test.content)
			if bytes.Equal(got[:], want[:]) {
				t.Fatalf("changing %s did not change upload request hash", test.name)
			}
		})
	}
}

func withUploadField(input UploadInput, field string) UploadInput {
	switch field {
	case "tenant":
		input.TenantID = "other-tenant"
	case "application":
		input.ApplicationID = "other-application"
	case "owner":
		input.OwnerUserID = "other-owner"
	case "request":
		input.RequestID = "other-request"
	case "name":
		input.OriginalName = "other.pdf"
	case "classification":
		input.Classification = "CONFIDENTIAL"
	}
	return input
}
