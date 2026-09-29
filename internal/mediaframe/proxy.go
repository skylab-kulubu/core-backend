package mediaframe

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// The proxy's failures: what the storage did, or the budget ran out.
var (
	errRedirect    = errors.New("the storage answered a redirect, which is never followed")
	errInputBudget = errors.New("reading a frame needs more of the video than the service reads")
)

// upstreamStatusError is the storage answering an error status.
type upstreamStatusError int

func (e upstreamStatusError) Error() string {
	return fmt.Sprintf("the storage answered %d", int(e))
}

// proxyPath is the one path the proxy serves: ffmpeg's input.
const proxyPath = "/video"

// upstreamProxy serves one video to ffmpeg on the loopback interface: it
// reads the video's allowed https address (and no other) with the Range
// ffmpeg asks for, follows no redirect, and stops once it has served
// budget bytes. Its first failure is what the request answers.
type upstreamProxy struct {
	target   string
	client   *http.Client
	listener net.Listener
	server   *http.Server
	budget   int64
	served   atomic.Int64

	mu    sync.Mutex
	first error
}

// startProxy serves target on 127.0.0.1, on a port of its own.
func startProxy(target *url.URL, transport http.RoundTripper, budget int64) (*upstreamProxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &upstreamProxy{
		target: target.String(),
		client: &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect },
		},
		listener: listener,
		budget:   budget,
	}
	p.server = &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 5 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go p.server.Serve(listener)
	return p, nil
}

// input is the address ffmpeg reads the video at.
func (p *upstreamProxy) input() string {
	return "http://" + p.listener.Addr().String() + proxyPath
}

func (p *upstreamProxy) close() { p.server.Close() }

// read is how many bytes of the video the proxy served.
func (p *upstreamProxy) read() int64 { return p.served.Load() }

func (p *upstreamProxy) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.first == nil {
		p.first = err
	}
}

// failure is the proxy's first failure; nil while it has none.
func (p *upstreamProxy) failure() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.first
}

func (p *upstreamProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != proxyPath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if p.served.Load() >= p.budget {
		p.fail(errInputBudget)
		http.Error(w, "budget", http.StatusBadGateway)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, p.target, nil)
	if err != nil {
		p.fail(err)
		http.Error(w, "request", http.StatusBadGateway)
		return
	}
	if byteRange := r.Header.Get("Range"); byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		if errors.Is(err, errRedirect) {
			p.fail(errRedirect)
		} else if r.Context().Err() == nil {
			// Never the address: it carries the signature.
			p.fail(errors.New("the storage could not be reached"))
		}
		http.Error(w, "upstream", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent, http.StatusRequestedRangeNotSatisfiable:
	default:
		p.fail(upstreamStatusError(resp.StatusCode))
		http.Error(w, "upstream", http.StatusBadGateway)
		return
	}
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	buf := make([]byte, 32<<10)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			allowed := min(int64(n), p.budget-p.served.Load())
			if allowed > 0 {
				p.served.Add(allowed)
				if _, err := w.Write(buf[:allowed]); err != nil {
					return
				}
			}
			if allowed < int64(n) {
				// Cut the connection: ffmpeg must see a failed read, never
				// a video that ends here.
				p.fail(errInputBudget)
				panic(http.ErrAbortHandler)
			}
		}
		if readErr != nil {
			return
		}
	}
}

// defaultTransport reads the video over https: bounded dial, handshake and
// header waits, and no proxy from the environment.
func defaultTransport() *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}
