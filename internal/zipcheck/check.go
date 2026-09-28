// Package zipcheck decides, before core streams a file to clamd, whether
// clamd would scan all of it (media redesign ticket 23). Checked against a
// real clamd 1.5.4, clamd skips without a report an archive member that
// inflates past its MaxFileSize, going by what the member really inflates
// to, not by the sizes its headers declare. It also skips a ZIP member with
// ZIP64 sizes or a compression method it does not know, and it unpacks
// archives it finds inside files (an Office package, a ZIP appended to a
// program, a PDF or an image) with the same limits. So a file passes only
// when everything clamd would unpack from it is within clamd's limits
// (CheckFile, CheckStream). A ZIP is read so (Check):
//
//   - its directory is read by bounded ranged reads (the end record, then
//     the central directory and what follows it, never more than
//     maxDirectoryBytes), and its counts and declared sizes are held to the
//     limits;
//   - the file is then streamed once, in order, and never kept: every
//     member's local header must agree with the directory, the members must
//     cover the file from its first byte to the directory with nothing
//     between or over them, and every member is inflated to prove it holds
//     exactly the bytes and checksum it declares;
//   - what each member holds is read as clamd would unpack it
//     (content.go): a ZIP inside is checked the same way, a gzip, bzip2 or
//     tar is opened and measured, an archive core cannot open is refused, and
//     a plain file is searched for archives embedded past its first byte.
//
// See "The ZIP check" in docs/media-lifecycle.md.
package zipcheck

import (
	"bufio"
	"context"
	"errors"
	"io"
	"time"
)

// Source is a stored file read by byte ranges (ranged GETs).
type Source interface {
	// OpenRange streams the n bytes of the file from off.
	OpenRange(ctx context.Context, off, n int64) (io.ReadCloser, error)
}

// ErrTookTooLong is a check that ran past its time (Timeout). It says
// nothing about the ZIP: the check can be made again.
var ErrTookTooLong = errors.New("zipcheck: the check ran past its time")

// Timeout is how long a check of a file of size bytes, whose members
// inflate to inflated bytes, may take: two minutes, two seconds per MiB of
// the file (it is read once, and an archive in it may be read again), and a
// second per MiB it inflates to.
func Timeout(size, inflated int64) time.Duration {
	return 2*time.Minute + 2*time.Second*time.Duration(size>>20) + time.Second*time.Duration(inflated>>20)
}

// MaxTimeout is the longest a check of a file of size bytes may take under
// the limits: its members never inflate to more than MaxScanSize.
func (l Limits) MaxTimeout(size int64) time.Duration {
	return Timeout(size, l.MaxScanSize)
}

// ErrZIPTooLargeToKeep is a ZIP read as a stream that is larger than
// Limits.MaxBuffer: the check reads a ZIP again from memory or by ranges,
// and a stream offers neither. It says nothing about the ZIP.
var ErrZIPTooLargeToKeep = errors.New("zipcheck: the ZIP is larger than the check keeps in memory, and cannot be read by ranges")

// CheckFile checks any file clamd will scan, reading src by ranges: a ZIP
// whole (Check), any other file once, as a stream, for the archives clamd
// would unpack from past its first byte. A ZIP there is checked as a nested
// one, ending the file (from its own range); any other archive there is
// refused as nested.
func CheckFile(ctx context.Context, src Source, size int64, limits Limits) error {
	return checkFile(ctx, src, size, limits, clock{start: time.Now(), now: time.Now})
}

func checkFile(ctx context.Context, src Source, size int64, limits Limits, clk clock) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	if size == 0 {
		return nil
	}
	head, err := readAt(ctx, src, 0, min(size, 4))
	if err != nil {
		return err
	}
	if IsZIP(head) {
		return check(ctx, src, size, limits, clk)
	}
	body, err := src.OpenRange(ctx, 0, size)
	if err != nil {
		return err
	}
	c := newChecker(ctx, limits, size, clk)
	var later []func() error
	err = c.inspect(newStream(body, size), file{at: "the file", size: size, src: src, later: func(check func() error) {
		later = append(later, check)
	}})
	body.Close()
	if err != nil {
		return err
	}
	for _, check := range later {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

// CheckStream checks any file clamd will scan, read once from r (a private
// file, decrypted as it is read; nothing is written anywhere): a ZIP is kept
// in memory to be checked whole, at most MaxBuffer (ErrZIPTooLargeToKeep
// beyond); any other file is searched for the archives clamd would unpack
// from past its first byte, a ZIP there kept in memory (a larger one is
// refused as nested).
func CheckStream(ctx context.Context, r io.Reader, size int64, limits Limits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	clk := clock{start: time.Now(), now: time.Now}
	br := bufio.NewReaderSize(r, headBytes)
	head, err := br.Peek(4)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if !IsZIP(head) {
		return newChecker(ctx, limits, size, clk).inspect(newStream(br, size), file{at: "the file", size: size})
	}
	if size > limits.MaxBuffer {
		return ErrZIPTooLargeToKeep
	}
	data, err := io.ReadAll(io.LimitReader(br, limits.MaxBuffer+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limits.MaxBuffer {
		return ErrZIPTooLargeToKeep
	}
	return check(ctx, Bytes(data), int64(len(data)), limits, clk)
}

// Check reads the ZIP of size bytes that src holds and returns nil when
// clamd would scan all of it, a *Refusal when it would not, or the error
// that stopped the check (storage, ctx, ErrTookTooLong), which says nothing
// about the ZIP. Its time grows with what the members inflate to, as the
// check learns it (Timeout).
//
// It reads the directory in ranges of at most 1 MiB, then streams the file
// once from its first byte; a stored ZIP inside is read again from its own
// range, a deflated one from memory (Limits.MaxBuffer).
func Check(ctx context.Context, src Source, size int64, limits Limits) error {
	return check(ctx, src, size, limits, clock{start: time.Now(), now: time.Now})
}

func check(ctx context.Context, src Source, size int64, limits Limits, clk clock) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	c := newChecker(ctx, limits, size, clk)
	dir, err := readDirectory(ctx, src, size, "", 0, c.b, 0)
	if err != nil {
		return err
	}
	return c.walkZIP(src, dir, "", 0)
}

// clock is when a check started, and the time now.
type clock struct {
	start time.Time
	now   func() time.Time
}

// checker is one check: the limits, what its members add up to, and its
// time.
type checker struct {
	ctx    context.Context
	limits Limits
	b      *budget
	clock  clock
	// size is the checked file's size.
	size int64
}

func newChecker(ctx context.Context, limits Limits, size int64, clk clock) *checker {
	return &checker{ctx: ctx, limits: limits, b: &budget{limits: limits}, clock: clk, size: size}
}

// alive stops a check its caller cancelled, or one past its time.
func (c *checker) alive() error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	if c.clock.now().Sub(c.clock.start) > Timeout(c.size, c.b.bytes) {
		return ErrTookTooLong
	}
	return nil
}
