package zipcheck

import (
	"errors"
	"fmt"
)

var (
	// ErrTooLarge refuses a ZIP that holds more than clamd scans whole:
	// clamd would skip part of it without a report.
	ErrTooLarge = errors.New("zipcheck: the ZIP holds more than clamd scans whole")
	// ErrInvalid refuses a ZIP that is malformed, or whose members clamd
	// cannot read.
	ErrInvalid = errors.New("zipcheck: the ZIP is malformed or holds what clamd cannot read")
	// ErrNested refuses a ZIP holding an archive core cannot check: one of
	// a format core cannot open (7-Zip, RAR, …), one nested too deep, or a
	// ZIP inside too large to read again from memory. Unpacked and uploaded
	// again, its files are checked.
	ErrNested = errors.New("zipcheck: the ZIP holds an archive core cannot check")
)

// Refusal is a ZIP the check refuses, and why. Reason names members by
// their place in the directory, never by name. errors.Is matches Err:
// ErrTooLarge, ErrInvalid or ErrNested.
type Refusal struct {
	Err    error
	Reason string
	// misplaced is a ZIP that is not where it seemed: no end record at the
	// end of the file, or a directory that is not where the end record
	// says. For a ZIP found past a file's first byte, it means no ZIP ends
	// the file there.
	misplaced bool
}

func (r *Refusal) Error() string { return r.Err.Error() + ": " + r.Reason }

func (r *Refusal) Unwrap() error { return r.Err }

func tooLarge(format string, args ...any) error {
	return &Refusal{Err: ErrTooLarge, Reason: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) error {
	return &Refusal{Err: ErrInvalid, Reason: fmt.Sprintf(format, args...)}
}

// misplaced refuses as invalid a ZIP that is not where it seemed
// (Refusal.misplaced).
func misplaced(format string, args ...any) error {
	return &Refusal{Err: ErrInvalid, Reason: fmt.Sprintf(format, args...), misplaced: true}
}

func nested(format string, args ...any) error {
	return &Refusal{Err: ErrNested, Reason: fmt.Sprintf(format, args...)}
}

// isRefusal reports whether err is a refusal.
func isRefusal(err error) bool {
	var r *Refusal
	return errors.As(err, &r)
}

// MaxEntries is the most members the check takes in one archive, whatever
// the limits say: the end record counts them in 16 bits, and ZIP64 counts
// are refused.
const MaxEntries = 1<<16 - 1

// MaxNesting is the deepest an archive may sit inside the ZIP: a ZIP in a
// ZIP in a ZIP in the uploaded one.
const MaxNesting = 3

// Limits are clamd's archive limits (clamd.conf), which a ZIP is held
// within, and how much of an archive core keeps in memory to check it.
// Every one must be set (Validate).
type Limits struct {
	// MaxFileSize is the most one member may inflate to, in bytes.
	MaxFileSize int64
	// MaxScanSize is the most all members together may inflate to.
	MaxScanSize int64
	// MaxFiles is the most members, at every level together; at most
	// MaxEntries.
	MaxFiles int
	// MaxRecursion is clamd's MaxRecursion: a file inside n archives is
	// scanned only while n < MaxRecursion (checked against clamd 1.5.4).
	// At least 2, so that clamd reaches the members of the ZIP itself.
	MaxRecursion int
	// MaxBuffer is core's own: the most bytes of a ZIP inside the ZIP
	// that core keeps in memory to read it again (a deflated one, one in a
	// gzip or tar, one embedded past a member's first byte).
	MaxBuffer int64
}

// Validate refuses limits the check cannot hold a ZIP within, one left out
// among them. Core validates its limits at startup.
func (l Limits) Validate() error {
	switch {
	case l.MaxFileSize < 1 || l.MaxScanSize < 1:
		return errors.New("zipcheck: MaxFileSize and MaxScanSize must be positive")
	case l.MaxFiles < 1 || l.MaxFiles > MaxEntries:
		return fmt.Errorf("zipcheck: MaxFiles must be from 1 to %d", MaxEntries)
	case l.MaxRecursion < 2:
		return errors.New("zipcheck: MaxRecursion must be at least 2, or clamd reaches no member of the ZIP")
	case l.MaxBuffer < 1:
		return errors.New("zipcheck: MaxBuffer must be positive")
	}
	return nil
}

// depth is how many archives deep inside the ZIP an archive may sit:
// MaxNesting, or less while clamd's MaxRecursion would not reach its
// members (a member of an archive n deep is inside n+1 archives, which
// clamd scans only while n+1 < MaxRecursion).
func (l Limits) depth() int {
	return min(MaxNesting, l.MaxRecursion-2)
}

// budget is what the members of the ZIP, and of the archives inside it,
// add up to against the limits.
type budget struct {
	limits Limits
	files  int
	bytes  int64
}

// count takes n members.
func (b *budget) count(n int) error {
	b.files += n
	if b.files > b.limits.MaxFiles {
		return tooLarge("%d members in all, more than MaxFiles (%d)", b.files, b.limits.MaxFiles)
	}
	return nil
}

// add takes a member that declares it inflates to usize bytes.
func (b *budget) add(member string, usize int64) error {
	if usize > b.limits.MaxFileSize {
		return tooLarge("%s inflates to %d bytes, more than MaxFileSize (%d)", member, usize, b.limits.MaxFileSize)
	}
	return b.grow(usize)
}

// grow takes n more inflated bytes.
func (b *budget) grow(n int64) error {
	b.bytes += n
	if b.bytes > b.limits.MaxScanSize {
		return tooLarge("the members inflate to more than MaxScanSize (%d) in all", b.limits.MaxScanSize)
	}
	return nil
}
