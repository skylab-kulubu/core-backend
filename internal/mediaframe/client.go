package mediaframe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrUnavailable is a frame service that cannot be reached, or answers that
// it is busy (503): nothing is wrong with the video, try again later.
var ErrUnavailable = errors.New("mediaframe: the frame service cannot be reached or is busy")

// ErrNoFrame is a video the service takes no frame from (422 no_frame):
// trying again does not help.
var ErrNoFrame = errors.New("mediaframe: the video gives no frame")

// Problem is the service's answer to a frame it did not take.
type Problem struct {
	Status int
	Code   string
	Detail string
	// UpstreamStatus is what the storage answered, with CodeUpstream.
	UpstreamStatus int
}

func (p *Problem) Error() string {
	out := fmt.Sprintf("the frame service answered %d %s", p.Status, p.Code)
	if p.Detail != "" {
		out += ": " + p.Detail
	}
	return out
}

// Is tells a video without a frame (ErrNoFrame) and a busy service
// (ErrUnavailable).
func (p *Problem) Is(target error) bool {
	switch target {
	case ErrNoFrame:
		return p.Status == http.StatusUnprocessableEntity && p.Code == CodeNoFrame
	case ErrUnavailable:
		return p.Status == http.StatusServiceUnavailable
	}
	return false
}

// Client is core's client of the frame service, on the internal network
// (MEDIA_FRAME_ADDR).
type Client struct {
	base string
	http *http.Client
}

// ParseAddr checks the service's address: host:port, as the ClamAV
// address is (the Dokploy application's appName and its port).
func ParseAddr(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	host, port, err := net.SplitHostPort(raw)
	if err != nil || host == "" || strings.ContainsAny(host, "/@:") {
		return "", fmt.Errorf("mediaframe: %q is not the frame service's host:port (such as media-frame:8080)", raw)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("mediaframe: %q names no port from 1 to 65535", raw)
	}
	return raw, nil
}

// NewClient is the client of the service at addr (host:port).
func NewClient(addr string) (*Client, error) {
	addr, err := ParseAddr(addr)
	if err != nil {
		return nil, err
	}
	return &Client{
		base: "http://" + addr,
		http: &http.Client{Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			ResponseHeaderTimeout: 2 * (DefaultTimeout + DefaultQueueWait),
			MaxIdleConns:          2,
			IdleConnTimeout:       30 * time.Second,
		}},
	}, nil
}

// Frame asks for the frame of the video at videoURL (a presigned GET) at
// at, and answers the JPEG. A problem the service answers is a *Problem; a
// service that cannot be reached is ErrUnavailable.
func (c *Client) Frame(ctx context.Context, videoURL string, at time.Duration) ([]byte, error) {
	body, err := json.Marshal(Request{URL: videoURL, AtMillis: at.Milliseconds()})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/frame", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// The error names the service's address, never the video's.
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, errors.Unwrap(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		problem := &Problem{Status: resp.StatusCode}
		var answered struct {
			Code           string `json:"code"`
			Detail         string `json:"detail"`
			UpstreamStatus int    `json:"upstreamStatus"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&answered) == nil {
			problem.Code, problem.Detail, problem.UpstreamStatus = answered.Code, answered.Detail, answered.UpstreamStatus
		}
		return nil, problem
	}
	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType != "image/jpeg" {
		return nil, fmt.Errorf("mediaframe: the frame service answered %q, not a JPEG", mediaType)
	}
	frame, err := io.ReadAll(io.LimitReader(resp.Body, DefaultMaxOutputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("mediaframe: read the frame: %w", err)
	}
	if len(frame) > DefaultMaxOutputBytes {
		return nil, fmt.Errorf("mediaframe: the frame is larger than %d bytes", DefaultMaxOutputBytes)
	}
	if !bytes.HasPrefix(frame, jpegStart) {
		return nil, errors.New("mediaframe: the frame service answered no JPEG")
	}
	return frame, nil
}
