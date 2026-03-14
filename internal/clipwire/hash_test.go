package clipwire

import "testing"

func TestComputeContentSHA256Deterministic(t *testing.T) {
	t.Parallel()

	first := ComputeContentSHA256([]byte("hello"))
	second := ComputeContentSHA256([]byte("hello"))
	third := ComputeContentSHA256([]byte("hellp"))

	if first != second {
		t.Fatal("expected identical hashes for identical input")
	}
	if first == third {
		t.Fatal("expected different hashes for different input")
	}
}

func TestComputeContentSHA256EmptyPayload(t *testing.T) {
	t.Parallel()

	first := ComputeContentSHA256(nil)
	second := ComputeContentSHA256([]byte{})

	if first != second {
		t.Fatal("expected nil and empty slice to hash identically")
	}
}
