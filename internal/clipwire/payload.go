package clipwire

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

const (
	// DefaultChunkSize keeps chunks at 1 MiB to avoid large websocket frames.
	DefaultChunkSize = 1 << 20
	// MaxPayloadBytes caps clipboard content at the planned 20 MB budget.
	MaxPayloadBytes = 20_000_000
)

var (
	ErrNilPayload          = errors.New("clipwire: payload is nil")
	ErrPayloadTooLarge     = errors.New("clipwire: payload exceeds maximum size")
	ErrInvalidPayloadType  = errors.New("clipwire: invalid payload type")
	ErrInvalidSHA256Length = errors.New("clipwire: sha256 must be 32 bytes")
	ErrInvalidSize         = errors.New("clipwire: size_bytes does not match data length")
	ErrHashMismatch        = errors.New("clipwire: sha256 does not match payload data")
	ErrInvalidUTF8         = errors.New("clipwire: text payload data must be valid UTF-8")
	ErrInvalidImageMIME    = errors.New("clipwire: image payload mime type must be image/png or image/jpeg")
	ErrUnexpectedMIMEType  = errors.New("clipwire: non-image payloads must not set mime_type")
)

var allowedImageMIMETypes = map[string]struct{}{
	"image/png":  {},
	"image/jpeg": {},
}

// NewPayload constructs a validated clipboard payload and computes its content hash.
func NewPayload(ts time.Time, kind PayloadType, data []byte, mimeType string) (*ClipboardPayload, error) {
	if kind == PayloadType_PAYLOAD_TYPE_UNSPECIFIED {
		return nil, ErrInvalidPayloadType
	}
	if len(data) > MaxPayloadBytes {
		return nil, ErrPayloadTooLarge
	}
	if kind == PayloadType_PAYLOAD_TYPE_IMAGE {
		if _, ok := allowedImageMIMETypes[mimeType]; !ok {
			return nil, ErrInvalidImageMIME
		}
	} else {
		if mimeType != "" {
			return nil, ErrUnexpectedMIMEType
		}
		if !utf8.Valid(data) {
			return nil, ErrInvalidUTF8
		}
	}

	sum := ComputeContentSHA256(data)
	hash := make([]byte, sha256.Size)
	copy(hash, sum[:])

	payload := &ClipboardPayload{
		TimestampUnixMs: ts.UTC().UnixMilli(),
		Type:            kind,
		Sha256:          hash,
		Data:            append([]byte(nil), data...),
		MimeType:        mimeType,
		SizeBytes:       uint64(len(data)),
	}

	if err := ValidatePayload(payload); err != nil {
		return nil, err
	}

	return payload, nil
}

// ValidatePayload verifies the payload fields and checks the content hash.
func ValidatePayload(p *ClipboardPayload) error {
	if p == nil {
		return ErrNilPayload
	}
	if p.GetType() == PayloadType_PAYLOAD_TYPE_UNSPECIFIED {
		return ErrInvalidPayloadType
	}
	if len(p.GetData()) > MaxPayloadBytes {
		return ErrPayloadTooLarge
	}
	if len(p.GetSha256()) != sha256.Size {
		return ErrInvalidSHA256Length
	}
	if p.GetSizeBytes() != uint64(len(p.GetData())) {
		return ErrInvalidSize
	}

	switch p.GetType() {
	case PayloadType_PAYLOAD_TYPE_IMAGE:
		if _, ok := allowedImageMIMETypes[p.GetMimeType()]; !ok {
			return ErrInvalidImageMIME
		}
	case PayloadType_PAYLOAD_TYPE_TEXT, PayloadType_PAYLOAD_TYPE_URL:
		if p.GetMimeType() != "" {
			return ErrUnexpectedMIMEType
		}
		if !utf8.Valid(p.GetData()) {
			return ErrInvalidUTF8
		}
	default:
		return ErrInvalidPayloadType
	}

	sum := ComputeContentSHA256(p.GetData())
	if !bytes.Equal(sum[:], p.GetSha256()) {
		return ErrHashMismatch
	}

	return nil
}

func copyBytes(src []byte) []byte {
	if len(src) == 0 {
		return nil
	}

	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}

func validateChunkSize(chunkSize int) error {
	if chunkSize <= 0 {
		return fmt.Errorf("clipwire: chunk size must be positive")
	}
	return nil
}
