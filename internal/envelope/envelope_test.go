package envelope_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/envelope"
)

func newKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

// objectKey is the associated data the tests seal under: the object's key.
var objectKey = []byte("private/files/cv")

func seal(t *testing.T, key, plaintext []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := envelope.NewWriter(&out, key, objectKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plaintext); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func open(key, ciphertext []byte) ([]byte, error) {
	return openAs(key, ciphertext, objectKey)
}

func openAs(key, ciphertext, associated []byte) ([]byte, error) {
	r, err := envelope.NewReader(bytes.NewReader(ciphertext), key, associated)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSealedFileOpensToTheSameBytes(t *testing.T) {
	t.Parallel()
	key := newKey(t)
	for _, size := range []int{0, 1, envelope.SegmentSize - 1, envelope.SegmentSize, envelope.SegmentSize + 1, 3*envelope.SegmentSize + 17} {
		plaintext := randomBytes(t, size)
		ciphertext := seal(t, key, plaintext)
		if bytes.Contains(ciphertext, plaintext) && size > 0 {
			t.Fatalf("%d bytes: the ciphertext carries the plaintext", size)
		}
		got, err := open(key, ciphertext)
		if err != nil {
			t.Fatalf("%d bytes: %v", size, err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Fatalf("%d bytes: opened %d different bytes", size, len(got))
		}
	}
}

func TestWriterStreamsSegmentsBeforeItIsClosed(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	w, err := envelope.NewWriter(&out, newKey(t), objectKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(randomBytes(t, 3*envelope.SegmentSize)); err != nil {
		t.Fatal(err)
	}
	// Two full segments are known not to be the last; the third waits for
	// Close, which decides whether it is.
	if out.Len() < 2*envelope.SegmentSize {
		t.Fatalf("only %d bytes written before Close", out.Len())
	}
}

func TestFlippedByteIsRefused(t *testing.T) {
	t.Parallel()
	key := newKey(t)
	plaintext := randomBytes(t, 2*envelope.SegmentSize+100)
	ciphertext := seal(t, key, plaintext)
	for name, at := range map[string]int{
		"header":             5,
		"first segment":      envelope.HeaderSize + 10,
		"second segment":     envelope.HeaderSize + envelope.SegmentSize + 16 + 10,
		"last segment's tag": len(ciphertext) - 1,
	} {
		tampered := bytes.Clone(ciphertext)
		tampered[at] ^= 0x01
		got, err := open(key, tampered)
		if !errors.Is(err, envelope.ErrCorrupt) {
			t.Errorf("%s: err = %v, want %v", name, err, envelope.ErrCorrupt)
		}
		// Nothing of a segment that fails its check is released.
		if !bytes.HasPrefix(plaintext, got) || len(got)%envelope.SegmentSize != 0 {
			t.Errorf("%s: released %d bytes", name, len(got))
		}
	}
}

func TestTruncatedOrExtendedCiphertextIsRefused(t *testing.T) {
	t.Parallel()
	key := newKey(t)
	ciphertext := seal(t, key, randomBytes(t, 2*envelope.SegmentSize+100))
	firstSegmentEnd := envelope.HeaderSize + envelope.SegmentSize + 16
	for name, tampered := range map[string][]byte{
		"cut after a whole segment": ciphertext[:firstSegmentEnd],
		"cut inside a segment":      ciphertext[:firstSegmentEnd+40],
		"header only":               ciphertext[:envelope.HeaderSize],
		"shorter than the header":   ciphertext[:3],
		"segment appended":          append(bytes.Clone(ciphertext), ciphertext[envelope.HeaderSize:firstSegmentEnd]...),
	} {
		if _, err := open(key, tampered); !errors.Is(err, envelope.ErrCorrupt) {
			t.Errorf("%s: err = %v, want %v", name, err, envelope.ErrCorrupt)
		}
	}
}

func TestAnotherKeyCannotOpenTheFile(t *testing.T) {
	t.Parallel()
	ciphertext := seal(t, newKey(t), []byte("an answer file"))
	if _, err := open(newKey(t), ciphertext); !errors.Is(err, envelope.ErrCorrupt) {
		t.Fatalf("err = %v, want %v", err, envelope.ErrCorrupt)
	}
}

func TestKeyMustBe256Bits(t *testing.T) {
	t.Parallel()
	if _, err := envelope.NewWriter(io.Discard, make([]byte, 16), objectKey); err == nil {
		t.Fatal("a 128-bit key was accepted for writing")
	}
	if _, err := envelope.NewReader(bytes.NewReader(nil), make([]byte, 16), objectKey); err == nil {
		t.Fatal("a 128-bit key was accepted for reading")
	}
}

// The ciphertext is bound to the object it was stored as: copied to another
// key, it does not open.
func TestFileSealedForOneObjectDoesNotOpenAsAnother(t *testing.T) {
	t.Parallel()
	key := newKey(t)
	ciphertext := seal(t, key, []byte("an answer file"))
	if _, err := openAs(key, ciphertext, []byte("private/files/other")); !errors.Is(err, envelope.ErrCorrupt) {
		t.Fatalf("err = %v, want %v", err, envelope.ErrCorrupt)
	}
}

// A segment that fails its check names its index, so a failure in the
// middle of a stream can be traced.
func TestCorruptSegmentIsNamed(t *testing.T) {
	t.Parallel()
	key := newKey(t)
	ciphertext := seal(t, key, randomBytes(t, 3*envelope.SegmentSize))
	ciphertext[envelope.HeaderSize+envelope.SegmentSize+16+10] ^= 0x01

	_, err := open(key, ciphertext)
	var corrupt *envelope.CorruptError
	if !errors.As(err, &corrupt) || corrupt.Segment != 1 {
		t.Fatalf("err = %v, want segment 1", err)
	}
}
