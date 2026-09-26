package handlers

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/clientip"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// contentStreamTimeout bounds how long one open of a read link may stream.
const contentStreamTimeout = 10 * time.Minute

// TrustProxies names the peers whose forwarded-for header may be read when
// an open of a read link is logged with the opener's address.
func (h *MediaHandler) TrustProxies(ranges clientip.Ranges) *MediaHandler {
	h.trustedProxies = ranges
	return h
}

// readLinkBody is the body of POST /v1/media/{id}/links.
type readLinkBody struct {
	OnBehalfOf string `json:"onBehalfOf"`
}

// IssueReadLink gives the owning product a five-minute read link to one of
// its private Media, for the person it acts for: 201 {url, expiresAt}.
func (h *MediaHandler) IssueReadLink(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return mediaError(c, err)
	}
	var body readLinkBody
	if err := c.Bind().JSON(&body); err != nil {
		body = readLinkBody{}
	}
	link, err := h.svc.IssueReadLink(c.Context(), p, parsedID(c.Params("id")), parsedID(body.OnBehalfOf))
	if err != nil {
		return h.error(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.Status(fiber.StatusCreated).JSON(link)
}

// Content opens a read link: GET /v1/media/{id}/content?token=…. It needs no
// sign-in; the token is the permission. The file is decrypted as it streams
// and always downloads, never renders.
func (h *MediaHandler) Content(c fiber.Ctx) error {
	token := c.Query("token")
	// A problem names the request it answers; it must not repeat the token.
	c.Request().URI().SetQueryString("")
	c.Set(fiber.HeaderCacheControl, "no-store")
	c.Set(fiber.HeaderReferrerPolicy, "no-referrer")
	id := parsedID(c.Params("id"))
	ctx, cancel := context.WithTimeout(c.Context(), contentStreamTimeout)
	content, err := h.svc.OpenContent(ctx, id, token, clientip.FromCtx(c, h.trustedProxies))
	if err != nil {
		cancel()
		return h.error(c, err)
	}
	contentType := content.Type
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	c.Set(fiber.HeaderContentType, contentType)
	c.Set(fiber.HeaderContentDisposition, attachmentDisposition(content.Name))
	c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
	c.Set(fiber.HeaderCacheControl, "private, no-store")
	c.Set(fiber.HeaderContentSecurityPolicy, "default-src 'none'; sandbox")
	return c.SendStream(&streamBody{body: content.Body, close: func() error {
		cancel()
		return content.Body.Close()
	}, failed: func(err error) {
		// The status is sent: the client only sees the download end short.
		h.logf("private media: GET /v1/media/%s/content: the stream ended early: %v", id, err)
	}}, int(content.Size))
}

// error answers an error of the private Media routes, logging what an
// operator must see.
func (h *MediaHandler) error(c fiber.Ctx, err error) error {
	if handled, problemErr := privateProblem(c, err, h.logf); handled {
		return problemErr
	}
	return mediaError(c, err)
}

// streamBody is the decrypting stream of a response. A read that fails is
// reported once; Close ends the stream's context once the response is sent.
type streamBody struct {
	body     io.Reader
	close    func() error
	failed   func(error)
	reported bool
}

func (b *streamBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && !b.reported {
		b.reported = true
		b.failed(err)
	}
	return n, err
}

func (b *streamBody) Close() error { return b.close() }

// attachmentDisposition is a download under the name, as the RFC 8187
// filename* parameter in UTF-8: every byte outside attr-char is percent
// encoded, so a name can neither break the header nor add to it.
func attachmentDisposition(name string) string {
	if name == "" {
		return "attachment"
	}
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.WriteString("attachment; filename*=UTF-8''")
	for i := 0; i < len(name); i++ {
		ch := name[i]
		if ('a' <= ch && ch <= 'z') || ('A' <= ch && ch <= 'Z') || ('0' <= ch && ch <= '9') || strings.IndexByte("!#$&+-.^_`|~", ch) >= 0 {
			b.WriteByte(ch)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[ch>>4])
		b.WriteByte(hex[ch&0x0f])
	}
	return b.String()
}

// privateProblem answers the errors of private Media and read links.
// handled is false for any other error.
func privateProblem(c fiber.Ctx, err error, logf func(string, ...any)) (handled bool, _ error) {
	switch {
	case errors.Is(err, media.ErrPrivateUnavailable):
		// The error names OpenBao's answer (the mount and key when they are
		// missing), never a token or a secret.
		logf("private media: %s %s: %v", c.Method(), c.Path(), err)
		c.Set(fiber.HeaderRetryAfter, "30")
		c.Set(fiber.HeaderCacheControl, "no-store")
		return true, problemDetailCode(c, fiber.StatusServiceUnavailable, "Service Unavailable",
			"Private Media storage cannot be reached right now. Public Media are not affected; retry later.", "private_media_unavailable")
	case errors.Is(err, media.ErrPrivateIntegrity):
		logf("private media: %s %s: %v", c.Method(), c.Path(), err)
		return true, problemDetailCode(c, fiber.StatusInternalServerError, "Internal Server Error",
			"The private Media failed its integrity check and is not served.", "private_media_integrity")
	case errors.Is(err, media.ErrPrivateMediaDisabled):
		return true, problemDetailCode(c, fiber.StatusUnprocessableEntity, "Unprocessable Content",
			"Private Media is not enabled.", "private_media_disabled")
	case errors.Is(err, media.ErrLinkForbidden):
		return true, problemDetailCode(c, fiber.StatusForbidden, "Forbidden",
			"Read links are for the owning product's service account with the media:attach role on the core client, and for privileged admins (core's own Media).", "media_link_forbidden")
	case errors.Is(err, media.ErrLinkSubjectInactive):
		return true, problemDetailCode(c, fiber.StatusUnprocessableEntity, "Unprocessable Content",
			"A read link is only issued for an active account: onBehalfOf is unknown to core or being erased.", "media_link_subject_inactive")
	case errors.Is(err, media.ErrLinkExpired):
		return true, problemDetailCode(c, fiber.StatusForbidden, "Forbidden",
			"The read link has expired; ask the product for a new one.", "media_link_expired")
	case errors.Is(err, media.ErrLinkInvalid):
		return true, problemDetailCode(c, fiber.StatusForbidden, "Forbidden",
			"The read link is not valid for this Media.", "media_link_invalid")
	default:
		return false, nil
	}
}
