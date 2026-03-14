package clipwire

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"

	"google.golang.org/protobuf/proto"
)

var (
	ErrNilChunk              = errors.New("clipwire: chunk is nil")
	ErrInvalidTransferID     = errors.New("clipwire: transfer_id must be 32 bytes")
	ErrInvalidChunkSequence  = errors.New("clipwire: sequence is outside the total_chunks range")
	ErrInvalidChunkCount     = errors.New("clipwire: total_chunks must be greater than zero")
	ErrInvalidChunkTotalSize = errors.New("clipwire: total_size must be greater than zero")
	ErrEmptyChunkData        = errors.New("clipwire: chunk data must not be empty")
	ErrChunkMetadataMismatch = errors.New("clipwire: chunk metadata does not match existing transfer state")
	ErrConflictingChunk      = errors.New("clipwire: duplicate chunk sequence has different data")
	ErrChunkSetSizeMismatch  = errors.New("clipwire: chunk sizes do not match declared total_size")
	ErrTransferHashMismatch  = errors.New("clipwire: reassembled payload hash does not match transfer_id")
)

// ChunkPayload serializes the payload and splits it into transport-safe chunks.
func ChunkPayload(p *ClipboardPayload, chunkSize int) ([]*PayloadChunk, error) {
	if err := ValidatePayload(p); err != nil {
		return nil, err
	}
	if err := validateChunkSize(chunkSize); err != nil {
		return nil, err
	}

	serialized, err := proto.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("clipwire: marshal payload: %w", err)
	}

	totalChunks := (len(serialized) + chunkSize - 1) / chunkSize
	chunks := make([]*PayloadChunk, 0, totalChunks)
	for i := 0; i < totalChunks; i++ {
		start := i * chunkSize
		end := min(start+chunkSize, len(serialized))

		chunks = append(chunks, &PayloadChunk{
			TransferId:  copyBytes(p.GetSha256()),
			Sequence:    uint32(i),
			TotalChunks: uint32(totalChunks),
			TotalSize:   uint64(len(serialized)),
			Data:        copyBytes(serialized[start:end]),
		})
	}

	return chunks, nil
}

// Reassembler tracks in-flight transfers and emits a payload once all chunks arrive.
type Reassembler struct {
	mu       sync.Mutex
	inflight map[string]*reassemblyState
}

type reassemblyState struct {
	totalChunks   uint32
	totalSize     uint64
	chunks        map[uint32][]byte
	receivedBytes uint64
}

// NewReassembler creates an empty chunk reassembler.
func NewReassembler() *Reassembler {
	return &Reassembler{
		inflight: make(map[string]*reassemblyState),
	}
}

// Add records a chunk and returns the payload once the transfer is complete.
func (r *Reassembler) Add(chunk *PayloadChunk) (*ClipboardPayload, bool, error) {
	if err := validateChunk(chunk); err != nil {
		return nil, false, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	key := string(chunk.GetTransferId())
	state, ok := r.inflight[key]
	if !ok {
		state = &reassemblyState{
			totalChunks: chunk.GetTotalChunks(),
			totalSize:   chunk.GetTotalSize(),
			chunks:      make(map[uint32][]byte, chunk.GetTotalChunks()),
		}
		r.inflight[key] = state
	} else if state.totalChunks != chunk.GetTotalChunks() || state.totalSize != chunk.GetTotalSize() {
		return nil, false, ErrChunkMetadataMismatch
	}

	if existing, exists := state.chunks[chunk.GetSequence()]; exists {
		if bytes.Equal(existing, chunk.GetData()) {
			return nil, false, nil
		}
		return nil, false, ErrConflictingChunk
	}

	data := copyBytes(chunk.GetData())
	state.chunks[chunk.GetSequence()] = data
	state.receivedBytes += uint64(len(data))

	if uint32(len(state.chunks)) != state.totalChunks {
		return nil, false, nil
	}
	if state.receivedBytes != state.totalSize {
		delete(r.inflight, key)
		return nil, false, ErrChunkSetSizeMismatch
	}

	serialized := make([]byte, 0, state.totalSize)
	for i := uint32(0); i < state.totalChunks; i++ {
		part, exists := state.chunks[i]
		if !exists {
			return nil, false, nil
		}
		serialized = append(serialized, part...)
	}
	delete(r.inflight, key)

	var payload ClipboardPayload
	if err := proto.Unmarshal(serialized, &payload); err != nil {
		return nil, false, fmt.Errorf("clipwire: unmarshal payload: %w", err)
	}
	if err := ValidatePayload(&payload); err != nil {
		return nil, false, err
	}
	if !bytes.Equal(payload.GetSha256(), chunk.GetTransferId()) {
		return nil, false, ErrTransferHashMismatch
	}

	return &payload, true, nil
}

func validateChunk(chunk *PayloadChunk) error {
	if chunk == nil {
		return ErrNilChunk
	}
	if len(chunk.GetTransferId()) != sha256.Size {
		return ErrInvalidTransferID
	}
	if chunk.GetTotalChunks() == 0 {
		return ErrInvalidChunkCount
	}
	if chunk.GetSequence() >= chunk.GetTotalChunks() {
		return ErrInvalidChunkSequence
	}
	if chunk.GetTotalSize() == 0 {
		return ErrInvalidChunkTotalSize
	}
	if len(chunk.GetData()) == 0 {
		return ErrEmptyChunkData
	}
	if uint64(len(chunk.GetData())) > chunk.GetTotalSize() {
		return ErrChunkSetSizeMismatch
	}

	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
