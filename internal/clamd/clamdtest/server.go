// Package clamdtest is a fake clamd for tests: it speaks the TCP protocol
// core uses (zPING, zVERSION and zINSTREAM with length-prefixed chunks, every
// reply NUL-terminated) and answers the way clamd does. A stream holding the
// EICAR test file is reported FOUND, as clamd reports it; anything else is
// clean. A test can make it answer an error, delay its answers, limit the
// stream length as clamd's StreamMaxLength does, or stop listening.
package clamdtest

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/clamd"
)

// Signature is the name the fake reports for the EICAR test file, as clamd
// 1.x does.
const Signature = "Eicar-Test-Signature"

// Version is what the fake answers to VERSION.
const Version = "ClamAV 1.5.4/28137/Mon Sep 28 06:24:12 2026"

// Server is the fake.
type Server struct {
	ln net.Listener
	t  testing.TB

	mu        sync.Mutex
	streamMax int64
	delay     time.Duration
	failure   string
	early     string
	truncated string
	found     map[string]string
	streams   [][]byte
	wg        sync.WaitGroup
	stopped   bool
}

// New starts the fake on a local port; it stops with the test.
func New(t testing.TB) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{ln: ln, t: t, found: map[string]string{string(clamd.EICAR()): Signature}}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(s.Stop)
	return s
}

// Addr is the fake's host:port, as MEDIA_CLAMAV_ADDR names clamd.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// LimitStream makes the fake refuse a stream longer than n bytes the way
// clamd refuses one over its StreamMaxLength.
func (s *Server) LimitStream(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamMax = n
}

// Delay makes every answer wait d: a slow or stuck clamd.
func (s *Server) Delay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = d
}

// Fail makes every scan answer "<reply> ERROR", as clamd does when it
// cannot scan; "" makes it scan again.
func (s *Server) Fail(reply string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failure = reply
}

// AnswerEarly makes the fake answer reply after the first chunk, before the
// stream's end, and then read the rest without answering again: a clamd
// that breaks the protocol.
func (s *Server) AnswerEarly(reply string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.early = reply
}

// AnswerTruncated makes the fake answer text without its NUL terminator
// and close the connection: an answer cut short.
func (s *Server) AnswerTruncated(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.truncated = text
}

// Report makes a stream containing marker FOUND as signature.
func (s *Server) Report(marker []byte, signature string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.found[string(marker)] = signature
}

// Streams are the streams the fake received whole, in order.
func (s *Server) Streams() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.streams...)
}

// Stop closes the listener: clamd is down, and connecting is refused.
func (s *Server) Stop() {
	s.mu.Lock()
	stopped := s.stopped
	s.stopped = true
	s.mu.Unlock()
	if stopped {
		return
	}
	_ = s.ln.Close()
	s.wg.Wait()
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer conn.Close()
			s.handle(conn)
		}()
	}
}

func (s *Server) handle(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(time.Minute))
	r := bufio.NewReader(conn)
	command, err := r.ReadString(0)
	if err != nil {
		return
	}
	s.mu.Lock()
	delay, failure, limit, early, truncated := s.delay, s.failure, s.streamMax, s.early, s.truncated
	s.mu.Unlock()
	reply := func(text string) {
		if delay > 0 {
			time.Sleep(delay)
		}
		_, _ = conn.Write([]byte(text + "\x00"))
	}
	switch strings.TrimSuffix(command, "\x00") {
	case "zPING":
		reply("PONG")
	case "zVERSION":
		reply(Version)
	case "zINSTREAM":
		var stream bytes.Buffer
		for chunks := 0; ; chunks++ {
			var size uint32
			if err := binary.Read(r, binary.BigEndian, &size); err != nil {
				return
			}
			if size == 0 {
				break
			}
			if chunks == 1 && early != "" {
				// After the first chunk, before the stream's end.
				reply(early)
			}
			if limit > 0 && int64(stream.Len())+int64(size) > limit {
				// clamd answers and closes without reading the rest.
				reply("INSTREAM size limit exceeded. ERROR")
				return
			}
			if _, err := io.CopyN(&stream, r, int64(size)); err != nil {
				return
			}
		}
		s.mu.Lock()
		s.streams = append(s.streams, stream.Bytes())
		signature := ""
		for marker, name := range s.found {
			if bytes.Contains(stream.Bytes(), []byte(marker)) {
				signature = name
			}
		}
		s.mu.Unlock()
		switch {
		case truncated != "":
			_, _ = conn.Write([]byte(truncated))
		case early != "":
			// Answered already.
		case failure != "":
			reply(failure + " ERROR")
		case signature != "":
			reply("stream: " + signature + " FOUND")
		default:
			reply("stream: OK")
		}
	default:
		reply("UNKNOWN COMMAND")
	}
}
