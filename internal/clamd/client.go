// Package clamd is a client of ClamAV's scanning daemon over TCP (media
// redesign ticket 12, ADR-0052). Core streams a file to clamd with INSTREAM,
// in length-prefixed chunks, and reads its verdict; nothing is written to
// disk on core's side. See "Malware scan" in docs/media-lifecycle.md.
package clamd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

var (
	// ErrUnreachable is a clamd core cannot connect to: down, restarting,
	// or not where MEDIA_CLAMAV_ADDR says. No file was scanned.
	ErrUnreachable = errors.New("clamd: unreachable")
	// ErrStreamTooLarge is a file longer than clamd takes in one stream
	// (its StreamMaxLength): clamd refused it without scanning it.
	ErrStreamTooLarge = errors.New("clamd: the file is longer than clamd's StreamMaxLength")
)

// ReplyError is an answer clamd gave instead of a verdict: it could not
// scan the file ("... ERROR"), or answered something core does not know.
type ReplyError struct {
	Reply string
}

func (e *ReplyError) Error() string { return "clamd answered " + fmt.Sprintf("%q", e.Reply) }

// Result is clamd's verdict on a file: the name of what it found, or
// nothing for a clean file.
type Result struct {
	Signature string
}

// Infected reports whether clamd found something.
func (r Result) Infected() bool { return r.Signature != "" }

const (
	// DefaultDialTimeout bounds connecting to clamd.
	DefaultDialTimeout = 5 * time.Second
	// DefaultIdleTimeout bounds a write that makes no progress: a clamd
	// that stopped reading.
	DefaultIdleTimeout = 2 * time.Minute
	// chunkSize is the most a chunk carries. clamd counts the whole stream
	// against StreamMaxLength, not the chunks.
	chunkSize = 64 << 10
	// maxReplyBytes bounds a reply; clamd's are a line.
	maxReplyBytes = 4 << 10
	// earlyReplyWait is how long a client whose write failed waits for the
	// answer clamd may have sent before it closed the connection.
	earlyReplyWait = 5 * time.Second
)

// Client talks to one clamd. Its zero timeouts take the defaults.
type Client struct {
	Addr        string
	DialTimeout time.Duration
	IdleTimeout time.Duration
}

// New is a client of the clamd at addr (host:port).
func New(addr string) *Client {
	return &Client{Addr: addr}
}

// Scan streams everything r yields to clamd and returns its verdict. The
// verdict is only for the whole file: a file r cannot read to its end is an
// error, the source's, and never a verdict. ctx bounds the whole scan,
// clamd's own time included.
func (c *Client) Scan(ctx context.Context, r io.Reader) (Result, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return Result{}, err
	}
	defer conn.Close()
	replies := make(chan reply, 1)
	go func() { replies <- readReply(conn) }()

	if err := c.write(conn, []byte("zINSTREAM\x00")); err != nil {
		return Result{}, c.afterWriteFailed(ctx, replies, err)
	}
	chunk := make([]byte, 4+chunkSize)
	for {
		n, readErr := r.Read(chunk[4:])
		if n > 0 {
			binary.BigEndian.PutUint32(chunk, uint32(n))
			if err := c.write(conn, chunk[:4+n]); err != nil {
				return Result{}, c.afterWriteFailed(ctx, replies, err)
			}
		}
		select {
		case early := <-replies:
			// clamd answers before the end only to refuse the stream.
			return verdict(early)
		default:
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return Result{}, fmt.Errorf("clamd: read the file to scan: %w", readErr)
		}
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
	}
	if err := c.write(conn, []byte{0, 0, 0, 0}); err != nil {
		return Result{}, c.afterWriteFailed(ctx, replies, err)
	}
	select {
	case answer := <-replies:
		return verdict(answer)
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// Ping checks clamd answers.
func (c *Client) Ping(ctx context.Context) error {
	answer, err := c.command(ctx, "zPING\x00")
	if err != nil {
		return err
	}
	if answer != "PONG" {
		return &ReplyError{Reply: answer}
	}
	return nil
}

// Version is clamd's version and its database's: "ClamAV 1.5.4/28137/<date>".
func (c *Client) Version(ctx context.Context) (string, error) {
	answer, err := c.command(ctx, "zVERSION\x00")
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(answer, "ClamAV ") {
		return "", &ReplyError{Reply: answer}
	}
	return answer, nil
}

func (c *Client) command(ctx context.Context, command string) (string, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := c.write(conn, []byte(command)); err != nil {
		return "", err
	}
	answer := readReply(conn)
	if answer.err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", answer.err
	}
	return answer.text, nil
}

// dial connects, and ties the connection to ctx: once ctx ends, every read
// and write on it fails at once.
func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	timeout := c.DialTimeout
	if timeout <= 0 {
		timeout = DefaultDialTimeout
	}
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", c.Addr)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	return &boundConn{Conn: conn, stop: stop}, nil
}

type boundConn struct {
	net.Conn
	stop func() bool
}

func (c *boundConn) Close() error {
	c.stop()
	return c.Conn.Close()
}

func (c *Client) write(conn net.Conn, data []byte) error {
	idle := c.IdleTimeout
	if idle <= 0 {
		idle = DefaultIdleTimeout
	}
	if err := conn.SetWriteDeadline(time.Now().Add(idle)); err != nil {
		return err
	}
	_, err := conn.Write(data)
	return err
}

// afterWriteFailed is the error of a scan whose write failed: clamd closes
// the connection after refusing a stream, so its answer may be waiting.
func (c *Client) afterWriteFailed(ctx context.Context, replies <-chan reply, writeErr error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(earlyReplyWait)
	defer timer.Stop()
	select {
	case answer := <-replies:
		if answer.err == nil {
			if _, err := verdict(answer); err != nil {
				return err
			}
		}
	case <-timer.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	return fmt.Errorf("clamd: the connection broke while sending the file: %w", writeErr)
}

type reply struct {
	text string
	err  error
}

// readReply reads one NUL-terminated answer (a connection closed after it
// ends it too).
func readReply(conn net.Conn) reply {
	var answer bytes.Buffer
	buf := make([]byte, 512)
	for answer.Len() < maxReplyBytes {
		n, err := conn.Read(buf)
		answer.Write(buf[:n])
		if i := bytes.IndexByte(answer.Bytes(), 0); i >= 0 {
			return reply{text: strings.TrimSpace(string(answer.Bytes()[:i]))}
		}
		if errors.Is(err, io.EOF) && answer.Len() > 0 {
			return reply{text: strings.TrimSpace(answer.String())}
		}
		if err != nil {
			return reply{err: fmt.Errorf("clamd: read the answer: %w", err)}
		}
	}
	return reply{err: &ReplyError{Reply: answer.String()[:64] + "…"}}
}

// verdict reads an INSTREAM answer: "stream: OK", "stream: <name> FOUND",
// or "<message> ERROR".
func verdict(answer reply) (Result, error) {
	if answer.err != nil {
		return Result{}, answer.err
	}
	text := answer.text
	switch {
	case text == "stream: OK":
		return Result{}, nil
	case strings.HasPrefix(text, "stream: ") && strings.HasSuffix(text, " FOUND"):
		signature := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "stream: "), " FOUND"))
		if signature == "" {
			return Result{}, &ReplyError{Reply: text}
		}
		return Result{Signature: signature}, nil
	case strings.Contains(text, "size limit exceeded"):
		return Result{}, fmt.Errorf("%w: %s", ErrStreamTooLarge, text)
	default:
		return Result{}, &ReplyError{Reply: text}
	}
}
