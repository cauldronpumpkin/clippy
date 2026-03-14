package clipwire

import (
	"bytes"
	"crypto/sha256"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
)

func TestChunkPayloadSingleChunk(t *testing.T) {
	t.Parallel()

	payload, err := NewPayload(time.Now(), PayloadType_PAYLOAD_TYPE_TEXT, []byte("hello"), "")
	if err != nil {
		t.Fatalf("NewPayload() error = %v", err)
	}

	chunks, err := ChunkPayload(payload, DefaultChunkSize)
	if err != nil {
		t.Fatalf("ChunkPayload() error = %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected one chunk, got %d", len(chunks))
	}
}

func TestChunkPayloadBoundaryAndUnevenSizes(t *testing.T) {
	t.Parallel()

	payload, err := NewPayload(time.Now(), PayloadType_PAYLOAD_TYPE_IMAGE, bytes.Repeat([]byte{0xaa}, DefaultChunkSize*2), "image/png")
	if err != nil {
		t.Fatalf("NewPayload() error = %v", err)
	}

	chunks, err := ChunkPayload(payload, DefaultChunkSize)
	if err != nil {
		t.Fatalf("ChunkPayload() error = %v", err)
	}
	if len(chunks) < 3 {
		t.Fatalf("expected protobuf overhead to produce at least three chunks, got %d", len(chunks))
	}

	payload, err = NewPayload(time.Now(), PayloadType_PAYLOAD_TYPE_IMAGE, bytes.Repeat([]byte{0xbb}, DefaultChunkSize+123), "image/png")
	if err != nil {
		t.Fatalf("NewPayload() error = %v", err)
	}
	chunks, err = ChunkPayload(payload, DefaultChunkSize)
	if err != nil {
		t.Fatalf("ChunkPayload() error = %v", err)
	}
	if got, want := len(chunks), 2; got != want {
		t.Fatalf("unexpected chunk count: got %d want %d", got, want)
	}
	if len(chunks[1].GetData()) >= DefaultChunkSize {
		t.Fatal("expected final chunk to be smaller than default chunk size")
	}
}

func TestChunkPayloadRejectsInvalidChunkSize(t *testing.T) {
	t.Parallel()

	payload, err := NewPayload(time.Now(), PayloadType_PAYLOAD_TYPE_TEXT, []byte("hello"), "")
	if err != nil {
		t.Fatalf("NewPayload() error = %v", err)
	}

	if _, err := ChunkPayload(payload, 0); err == nil {
		t.Fatal("expected invalid chunk size to fail")
	}
}

func TestReassemblerOutOfOrderSuccess(t *testing.T) {
	t.Parallel()

	payload, err := NewPayload(time.Now(), PayloadType_PAYLOAD_TYPE_IMAGE, bytes.Repeat([]byte{0xcc}, DefaultChunkSize+321), "image/jpeg")
	if err != nil {
		t.Fatalf("NewPayload() error = %v", err)
	}

	chunks, err := ChunkPayload(payload, DefaultChunkSize)
	if err != nil {
		t.Fatalf("ChunkPayload() error = %v", err)
	}

	reassembler := NewReassembler()
	var restored *ClipboardPayload
	var complete bool
	for i := len(chunks) - 1; i >= 0; i-- {
		restored, complete, err = reassembler.Add(chunks[i])
		if err != nil {
			t.Fatalf("Add() error = %v", err)
		}
	}
	if !complete {
		t.Fatal("expected reassembly to complete")
	}
	if !proto.Equal(restored, payload) {
		t.Fatal("reassembled payload does not match original")
	}
}

func TestReassemblerDuplicateIdenticalChunkIsIgnored(t *testing.T) {
	t.Parallel()

	payload, err := NewPayload(time.Now(), PayloadType_PAYLOAD_TYPE_IMAGE, bytes.Repeat([]byte{0xa1}, DefaultChunkSize+11), "image/png")
	if err != nil {
		t.Fatalf("NewPayload() error = %v", err)
	}
	chunks, err := ChunkPayload(payload, DefaultChunkSize)
	if err != nil {
		t.Fatalf("ChunkPayload() error = %v", err)
	}

	reassembler := NewReassembler()
	if _, complete, err := reassembler.Add(chunks[0]); err != nil || complete {
		t.Fatalf("first Add() = complete %v, err %v", complete, err)
	}
	if _, complete, err := reassembler.Add(chunks[0]); err != nil || complete {
		t.Fatalf("duplicate Add() = complete %v, err %v", complete, err)
	}
}

func TestReassemblerMissingChunkDoesNotComplete(t *testing.T) {
	t.Parallel()

	payload, err := NewPayload(time.Now(), PayloadType_PAYLOAD_TYPE_IMAGE, bytes.Repeat([]byte{0xdd}, DefaultChunkSize+10), "image/png")
	if err != nil {
		t.Fatalf("NewPayload() error = %v", err)
	}
	chunks, err := ChunkPayload(payload, DefaultChunkSize)
	if err != nil {
		t.Fatalf("ChunkPayload() error = %v", err)
	}

	reassembler := NewReassembler()
	for i := 0; i < len(chunks)-1; i++ {
		if _, complete, err := reassembler.Add(chunks[i]); err != nil {
			t.Fatalf("Add() error = %v", err)
		} else if complete {
			t.Fatal("did not expect reassembly to complete early")
		}
	}
}

func TestReassemblerRejectsConflictingDuplicateChunk(t *testing.T) {
	t.Parallel()

	payload, err := NewPayload(time.Now(), PayloadType_PAYLOAD_TYPE_IMAGE, bytes.Repeat([]byte{0xee}, DefaultChunkSize+10), "image/png")
	if err != nil {
		t.Fatalf("NewPayload() error = %v", err)
	}
	chunks, err := ChunkPayload(payload, DefaultChunkSize)
	if err != nil {
		t.Fatalf("ChunkPayload() error = %v", err)
	}

	reassembler := NewReassembler()
	if _, _, err := reassembler.Add(chunks[0]); err != nil {
		t.Fatalf("initial Add() error = %v", err)
	}

	conflict := proto.Clone(chunks[0]).(*PayloadChunk)
	conflict.Data = append([]byte(nil), conflict.GetData()...)
	conflict.Data[0] ^= 0xff

	if _, _, err := reassembler.Add(conflict); err == nil {
		t.Fatal("expected conflicting duplicate to fail")
	}
}

func TestReassemblerRejectsMetadataMismatch(t *testing.T) {
	t.Parallel()

	payload, err := NewPayload(time.Now(), PayloadType_PAYLOAD_TYPE_IMAGE, bytes.Repeat([]byte{0xab}, DefaultChunkSize+10), "image/jpeg")
	if err != nil {
		t.Fatalf("NewPayload() error = %v", err)
	}
	chunks, err := ChunkPayload(payload, DefaultChunkSize)
	if err != nil {
		t.Fatalf("ChunkPayload() error = %v", err)
	}

	reassembler := NewReassembler()
	if _, _, err := reassembler.Add(chunks[0]); err != nil {
		t.Fatalf("initial Add() error = %v", err)
	}

	mismatch := proto.Clone(chunks[1]).(*PayloadChunk)
	mismatch.TotalSize++

	if _, _, err := reassembler.Add(mismatch); err == nil {
		t.Fatal("expected metadata mismatch to fail")
	}
}

func TestReassemblerRejectsCorruptedChunkData(t *testing.T) {
	t.Parallel()

	payload, err := NewPayload(time.Now(), PayloadType_PAYLOAD_TYPE_IMAGE, bytes.Repeat([]byte{0xfa}, DefaultChunkSize+44), "image/png")
	if err != nil {
		t.Fatalf("NewPayload() error = %v", err)
	}
	chunks, err := ChunkPayload(payload, DefaultChunkSize)
	if err != nil {
		t.Fatalf("ChunkPayload() error = %v", err)
	}

	corrupted := proto.Clone(chunks[len(chunks)-1]).(*PayloadChunk)
	corrupted.Data = append([]byte(nil), corrupted.GetData()...)
	corrupted.Data[len(corrupted.Data)-1] ^= 0x01

	reassembler := NewReassembler()
	for i := 0; i < len(chunks)-1; i++ {
		if _, _, err := reassembler.Add(chunks[i]); err != nil {
			t.Fatalf("Add() error = %v", err)
		}
	}

	if _, _, err := reassembler.Add(corrupted); err == nil {
		t.Fatal("expected corrupted chunk data to fail")
	}
}

func TestSerializationRoundTrip(t *testing.T) {
	t.Parallel()

	payload, err := NewPayload(time.UnixMilli(12345), PayloadType_PAYLOAD_TYPE_URL, []byte("https://example.com/test"), "")
	if err != nil {
		t.Fatalf("NewPayload() error = %v", err)
	}

	encoded, err := proto.Marshal(payload)
	if err != nil {
		t.Fatalf("proto.Marshal() error = %v", err)
	}

	var decoded ClipboardPayload
	if err := proto.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("proto.Unmarshal() error = %v", err)
	}
	if !proto.Equal(payload, &decoded) {
		t.Fatal("decoded payload does not match original")
	}
}

func TestValidateChunkRejectsBadTransferID(t *testing.T) {
	t.Parallel()

	chunk := &PayloadChunk{
		TransferId:  []byte("short"),
		Sequence:    0,
		TotalChunks: 1,
		TotalSize:   1,
		Data:        []byte{1},
	}

	if err := validateChunk(chunk); err == nil {
		t.Fatal("expected invalid transfer ID to fail")
	}
}

func TestValidatePayloadRejectsTransferHashMismatchAfterReassembly(t *testing.T) {
	t.Parallel()

	payload, err := NewPayload(time.Now(), PayloadType_PAYLOAD_TYPE_TEXT, []byte("hash-test"), "")
	if err != nil {
		t.Fatalf("NewPayload() error = %v", err)
	}
	chunks, err := ChunkPayload(payload, DefaultChunkSize)
	if err != nil {
		t.Fatalf("ChunkPayload() error = %v", err)
	}
	chunks[0].TransferId = make([]byte, sha256.Size)

	reassembler := NewReassembler()
	if _, _, err := reassembler.Add(chunks[0]); err == nil {
		t.Fatal("expected transfer hash mismatch to fail after completion")
	}
}
