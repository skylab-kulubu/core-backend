package clamd_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/clamd"
	"github.com/skylab-kulubu/core-backend/internal/clamd/clamdtest"
)

func scan(t *testing.T, client *clamd.Client, data any) (clamd.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, ok := data.(io.Reader)
	if !ok {
		r = bytes.NewReader(data.([]byte))
	}
	return client.Scan(ctx, r)
}

func TestScanStreamsTheWholeFileInChunksAndAnswersClean(t *testing.T) {
	fake := clamdtest.New(t)
	// Several chunks, the last one short.
	file := bytes.Repeat([]byte("%PDF-1.7 not a virus "), 10_000)

	result, err := scan(t, clamd.New(fake.Addr()), file)
	if err != nil {
		t.Fatal(err)
	}
	if result.Infected() || result.Signature != "" {
		t.Fatalf("clean file reported %+v", result)
	}
	streams := fake.Streams()
	if len(streams) != 1 || !bytes.Equal(streams[0], file) {
		t.Fatalf("clamd received %d streams, the first %d bytes; want the %d-byte file", len(streams), len(first(streams)), len(file))
	}
}

func first(streams [][]byte) []byte {
	if len(streams) == 0 {
		return nil
	}
	return streams[0]
}

func TestScanReportsWhatClamdFound(t *testing.T) {
	fake := clamdtest.New(t)
	file := append([]byte("%PDF-1.7\n"), clamd.EICAR()...)

	result, err := scan(t, clamd.New(fake.Addr()), file)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Infected() || result.Signature != clamdtest.Signature {
		t.Fatalf("infected file reported %+v", result)
	}
}

func TestScanReturnsClamdsErrorAnswer(t *testing.T) {
	fake := clamdtest.New(t)
	fake.Fail("stream: Can't allocate memory")

	_, err := scan(t, clamd.New(fake.Addr()), []byte("anything"))
	var reply *clamd.ReplyError
	if !errors.As(err, &reply) || !strings.Contains(reply.Reply, "Can't allocate memory") {
		t.Fatalf("err = %v, want clamd's ERROR answer", err)
	}
	if errors.Is(err, clamd.ErrUnreachable) || errors.Is(err, clamd.ErrStreamTooLarge) {
		t.Fatalf("an ERROR answer is neither unreachable nor too large: %v", err)
	}
}

// clamd refuses a stream over its StreamMaxLength and closes the
// connection without reading the rest: the client stops sending and tells
// the file was too large to scan.
func TestScanTellsAStreamLongerThanClamdTakes(t *testing.T) {
	fake := clamdtest.New(t)
	fake.LimitStream(64 << 10)

	_, err := scan(t, clamd.New(fake.Addr()), bytes.Repeat([]byte{'x'}, 8<<20))
	if !errors.Is(err, clamd.ErrStreamTooLarge) {
		t.Fatalf("err = %v, want %v", err, clamd.ErrStreamTooLarge)
	}
	if len(fake.Streams()) != 0 {
		t.Fatal("clamd scanned a stream it refused")
	}
}

func TestScanOfAClamdThatIsDownIsUnreachable(t *testing.T) {
	fake := clamdtest.New(t)
	addr := fake.Addr()
	fake.Stop()

	_, err := scan(t, clamd.New(addr), []byte("anything"))
	if !errors.Is(err, clamd.ErrUnreachable) {
		t.Fatalf("err = %v, want %v", err, clamd.ErrUnreachable)
	}
}

// A clamd that does not answer in time ends the scan with the context's
// error: the file is neither clean nor infected.
func TestScanOfASlowClamdEndsWithTheContext(t *testing.T) {
	fake := clamdtest.New(t)
	fake.Delay(time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := clamd.New(fake.Addr()).Scan(ctx, bytes.NewReader([]byte("anything")))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want %v", err, context.DeadlineExceeded)
	}
	if errors.Is(err, clamd.ErrUnreachable) {
		t.Fatalf("a slow clamd is reachable: %v", err)
	}
	if waited := time.Since(started); waited > 900*time.Millisecond {
		t.Fatalf("the scan waited %v past its context", waited)
	}
}

// A file that cannot be read to its end is not scanned: the error is the
// source's, and clamd gives no verdict.
func TestScanOfAFileThatFailsToReadIsNotAVerdict(t *testing.T) {
	fake := clamdtest.New(t)
	broken := errors.New("storage: connection reset")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := clamd.New(fake.Addr()).Scan(ctx, io.MultiReader(bytes.NewReader([]byte("start")), failingReader{broken}))
	if !errors.Is(err, broken) {
		t.Fatalf("err = %v, want the source's %v", err, broken)
	}
	if len(fake.Streams()) != 0 {
		t.Fatal("clamd scanned part of a file")
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestPingAndVersion(t *testing.T) {
	fake := clamdtest.New(t)
	client := clamd.New(fake.Addr())
	ctx := context.Background()
	if err := client.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	version, err := client.Version(ctx)
	if err != nil || version != clamdtest.Version {
		t.Fatalf("version %q, err %v", version, err)
	}
}

// The deploy check: clamd must report the EICAR test file.
func TestSelfTestPassesOnlyWhenClamdReportsEICAR(t *testing.T) {
	fake := clamdtest.New(t)
	ctx := context.Background()

	got, err := clamd.SelfTest(ctx, clamd.New(fake.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	if got.Signature != clamdtest.Signature || got.Version != clamdtest.Version {
		t.Fatalf("self-test %+v", got)
	}

	// A clamd whose database does not know EICAR fails it.
	blind := clamdtest.New(t)
	blind.Report(clamd.EICAR(), "")
	if _, err := clamd.SelfTest(ctx, clamd.New(blind.Addr())); !errors.Is(err, clamd.ErrSelfTestMissed) {
		t.Fatalf("clean answer: err = %v, want %v", err, clamd.ErrSelfTestMissed)
	}

	down := clamdtest.New(t)
	down.Stop()
	if _, err := clamd.SelfTest(ctx, clamd.New(down.Addr())); !errors.Is(err, clamd.ErrUnreachable) {
		t.Fatalf("down: err = %v, want %v", err, clamd.ErrUnreachable)
	}
}

// EICAR is the 68-byte test file every scanner reports.
func TestEICARIsTheStandardTestFile(t *testing.T) {
	eicar := clamd.EICAR()
	if len(eicar) != 68 || !bytes.HasPrefix(eicar, []byte("X5O!P%@AP[4")) || !bytes.HasSuffix(eicar, []byte("!$H+H*")) {
		t.Fatalf("EICAR is %d bytes: %q", len(eicar), eicar)
	}
}

// A verdict counts only once the whole file is sent and clamd has answered
// once, after the end of the stream. clamd answers before the end only to
// refuse the stream: an early "OK" (or FOUND) is a protocol failure and
// never clean, even when the file's last chunk holds EICAR.
func TestScanNeverTakesAnAnswerBeforeTheEndAsAVerdict(t *testing.T) {
	file := append(bytes.Repeat([]byte("A"), 1<<20), clamd.EICAR()...)
	for _, early := range []string{"stream: OK", "stream: Eicar-Test-Signature FOUND", "PONG"} {
		fake := clamdtest.New(t)
		fake.AnswerEarly(early)
		result, err := scan(t, clamd.New(fake.Addr()), &slowReader{r: bytes.NewReader(file), wait: 2 * time.Millisecond})
		var reply *clamd.ReplyError
		if !errors.As(err, &reply) || !errors.Is(err, clamd.ErrProtocol) {
			t.Errorf("early %q: result %+v, err %v; want a protocol failure", early, result, err)
		}
	}
	// An early refusal is still the failure it names.
	fake := clamdtest.New(t)
	fake.AnswerEarly("stream: Can't allocate memory ERROR")
	_, err := scan(t, clamd.New(fake.Addr()), &slowReader{r: bytes.NewReader(file), wait: 2 * time.Millisecond})
	var reply *clamd.ReplyError
	if !errors.As(err, &reply) || !strings.Contains(reply.Reply, "Can't allocate memory") || errors.Is(err, clamd.ErrProtocol) {
		t.Fatalf("early ERROR: err = %v", err)
	}
}

type slowReader struct {
	r    io.Reader
	wait time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	time.Sleep(s.wait)
	return s.r.Read(p)
}

// An answer cut short (no NUL terminator) is no verdict, even when what
// arrived reads "stream: OK".
func TestScanRefusesAnAnswerWithoutItsTerminator(t *testing.T) {
	for _, text := range []string{"stream: OK", "stream: O"} {
		fake := clamdtest.New(t)
		fake.AnswerTruncated(text)
		result, err := scan(t, clamd.New(fake.Addr()), []byte("anything"))
		if err == nil || result.Infected() {
			t.Errorf("truncated %q: result %+v, err %v; want an error", text, result, err)
		}
	}
}
