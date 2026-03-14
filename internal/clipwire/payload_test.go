package clipwire

import (
	"crypto/sha256"
	"testing"
	"time"
)

func TestNewPayloadKinds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		kind     PayloadType
		data     []byte
		mimeType string
	}{
		{name: "text", kind: PayloadType_PAYLOAD_TYPE_TEXT, data: []byte("hello")},
		{name: "url", kind: PayloadType_PAYLOAD_TYPE_URL, data: []byte("https://example.com")},
		{name: "image", kind: PayloadType_PAYLOAD_TYPE_IMAGE, data: []byte{0xff, 0xd8, 0xff}, mimeType: "image/jpeg"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			payload, err := NewPayload(time.UnixMilli(1234), tc.kind, tc.data, tc.mimeType)
			if err != nil {
				t.Fatalf("NewPayload() error = %v", err)
			}
			if payload.GetSizeBytes() != uint64(len(tc.data)) {
				t.Fatalf("unexpected size_bytes: got %d want %d", payload.GetSizeBytes(), len(tc.data))
			}
			if err := ValidatePayload(payload); err != nil {
				t.Fatalf("ValidatePayload() error = %v", err)
			}
		})
	}
}

func TestNewPayloadRejectsInvalidImageMimeType(t *testing.T) {
	t.Parallel()

	if _, err := NewPayload(time.Now(), PayloadType_PAYLOAD_TYPE_IMAGE, []byte("abc"), ""); err == nil {
		t.Fatal("expected missing image mime type to fail")
	}
}

func TestValidatePayloadRejectsInvalidHashLength(t *testing.T) {
	t.Parallel()

	payload := &ClipboardPayload{
		TimestampUnixMs: time.Now().UnixMilli(),
		Type:            PayloadType_PAYLOAD_TYPE_TEXT,
		Sha256:          []byte("short"),
		Data:            []byte("hello"),
		SizeBytes:       5,
	}

	if err := ValidatePayload(payload); err == nil {
		t.Fatal("expected invalid hash length to fail")
	}
}

func TestValidatePayloadRejectsMismatchedSize(t *testing.T) {
	t.Parallel()

	sum := ComputeContentSHA256([]byte("hello"))
	payload := &ClipboardPayload{
		TimestampUnixMs: time.Now().UnixMilli(),
		Type:            PayloadType_PAYLOAD_TYPE_TEXT,
		Sha256:          sum[:],
		Data:            []byte("hello"),
		SizeBytes:       99,
	}

	if err := ValidatePayload(payload); err == nil {
		t.Fatal("expected mismatched size to fail")
	}
}

func TestNewPayloadRejectsOversizedData(t *testing.T) {
	t.Parallel()

	data := make([]byte, MaxPayloadBytes+1)
	if _, err := NewPayload(time.Now(), PayloadType_PAYLOAD_TYPE_IMAGE, data, "image/png"); err == nil {
		t.Fatal("expected oversized payload to fail")
	}
}

func TestValidatePayloadRejectsHashMismatch(t *testing.T) {
	t.Parallel()

	badHash := make([]byte, sha256.Size)
	copy(badHash, []byte("not-the-real-hash"))

	payload := &ClipboardPayload{
		TimestampUnixMs: time.Now().UnixMilli(),
		Type:            PayloadType_PAYLOAD_TYPE_TEXT,
		Sha256:          badHash,
		Data:            []byte("hello"),
		SizeBytes:       5,
	}

	if err := ValidatePayload(payload); err == nil {
		t.Fatal("expected hash mismatch to fail")
	}
}
