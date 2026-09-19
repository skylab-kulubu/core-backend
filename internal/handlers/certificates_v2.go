package handlers

import (
	"html"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
)

func (h *CertificateHandler) PublicPage(c fiber.Ctx) error {
	item, err := h.svc.Verify(c.Context(), c.Params("serial"))
	if err != nil {
		return certError(c, err)
	}
	valid := item.Status == "valid"
	statusTitle, statusText, statusClass := "Geçerli sertifika", "Bu sertifika SKY LAB kayıtlarında geçerlidir.", "valid"
	if !valid {
		statusTitle, statusText, statusClass = "İptal edilmiş sertifika", "Bu sertifika artık geçerli değildir.", "revoked"
	}
	team := item.OwnerTeam
	if team == "YK" || team == "DK" || team == "" {
		team = "SKY LAB"
	}
	page := `<!doctype html><html lang="tr"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Sertifika doğrulama · SKY LAB</title><style>
:root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;padding:24px;background:radial-gradient(circle at top,#312e81 0,#111827 40%,#09090b 100%);font-family:Inter,ui-sans-serif,system-ui;color:#f8fafc}.card{width:min(680px,100%);padding:36px;border:1px solid rgba(255,255,255,.14);border-radius:28px;background:rgba(17,24,39,.82);box-shadow:0 30px 80px rgba(0,0,0,.4);backdrop-filter:blur(16px)}.brand{font-weight:800;letter-spacing:.18em;font-size:13px;color:#c4b5fd}.badge{display:inline-flex;margin-top:28px;padding:8px 12px;border-radius:999px;font-weight:700;font-size:14px}.valid{background:#064e3b;color:#a7f3d0}.revoked{background:#7f1d1d;color:#fecaca}h1{font-size:clamp(30px,6vw,52px);line-height:1.05;margin:24px 0 12px}h2{font-size:20px;color:#cbd5e1;font-weight:500;margin:0}.meta{display:grid;grid-template-columns:1fr 1fr;gap:18px;margin-top:32px;padding-top:24px;border-top:1px solid rgba(255,255,255,.12)}.label{font-size:12px;text-transform:uppercase;letter-spacing:.08em;color:#94a3b8}.value{margin-top:6px;font-weight:650;overflow-wrap:anywhere}.actions{margin-top:28px}.button{display:inline-flex;padding:12px 18px;border-radius:12px;background:#fff;color:#111827;text-decoration:none;font-weight:750}@media(max-width:520px){.card{padding:24px}.meta{grid-template-columns:1fr}}
</style></head><body><main class="card"><div class="brand">SKY LAB</div><div class="badge ` + statusClass + `">` + html.EscapeString(statusTitle) + `</div><h1>` + html.EscapeString(item.RecipientName) + `</h1><h2>` + html.EscapeString(item.EventName) + `</h2><p>` + html.EscapeString(statusText) + `</p><section class="meta"><div><div class="label">Düzenleyen</div><div class="value">` + html.EscapeString(team) + `</div></div><div><div class="label">Verilme tarihi</div><div class="value">` + item.IssuedAt.Format("02.01.2006") + `</div></div><div><div class="label">Sertifika kodu</div><div class="value">` + html.EscapeString(item.Serial) + `</div></div><div><div class="label">Durum</div><div class="value">` + html.EscapeString(statusTitle) + `</div></div></section>`
	if valid {
		page += `<div class="actions"><a class="button" href="` + html.EscapeString(item.PDFURL) + `">PDF sertifikayı indir</a></div>`
	}
	page += `</main></body></html>`
	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
	return c.SendString(page)
}

func (h *CertificateHandler) Summary(c fiber.Ctx) error {
	p, eventID, err := certificateEventCaller(c)
	if err != nil {
		return certError(c, err)
	}
	result, err := h.svc.Summary(c.Context(), p, eventID)
	if err != nil {
		return certError(c, err)
	}
	return c.JSON(result)
}

func (h *CertificateHandler) Finalize(c fiber.Ctx) error {
	p, eventID, err := certificateEventCaller(c)
	if err != nil {
		return certError(c, err)
	}
	result, err := h.svc.Finalize(c.Context(), p, eventID)
	if err != nil {
		return certError(c, err)
	}
	return c.Status(fiber.StatusAccepted).JSON(result)
}

func (h *CertificateHandler) Resolve(c fiber.Ctx) error {
	p, eventID, err := certificateEventCaller(c)
	if err != nil {
		return certError(c, err)
	}
	result, err := h.svc.ResolveTemplate(c.Context(), p, eventID)
	if err != nil {
		return certError(c, err)
	}
	return c.JSON(result)
}

func (h *CertificateHandler) PreviewEvent(c fiber.Ctx) error {
	p, eventID, err := certificateEventCaller(c)
	if err != nil {
		return certError(c, err)
	}
	pdf, err := h.svc.PreviewEvent(c.Context(), p, eventID)
	if err != nil {
		return certError(c, err)
	}
	c.Set(fiber.HeaderContentType, "application/pdf")
	return c.Send(pdf)
}

func (h *CertificateHandler) ListBatches(c fiber.Ctx) error {
	p, eventID, err := certificateEventCaller(c)
	if err != nil {
		return certError(c, err)
	}
	result, err := h.svc.ListBatches(c.Context(), p, eventID)
	if err != nil {
		return certError(c, err)
	}
	return c.JSON(result)
}

func (h *CertificateHandler) GetBatch(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return certError(c, certificate.ErrInvalid)
	}
	result, err := h.svc.GetBatch(c.Context(), p, id)
	if err != nil {
		return certError(c, err)
	}
	return c.JSON(result)
}

func (h *CertificateHandler) RetryBatch(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return certError(c, certificate.ErrInvalid)
	}
	result, err := h.svc.RetryBatch(c.Context(), p, id)
	if err != nil {
		return certError(c, err)
	}
	return c.Status(fiber.StatusAccepted).JSON(result)
}

func (h *CertificateHandler) Reissue(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	result, err := h.svc.Reissue(c.Context(), p, c.Params("serial"))
	if err != nil {
		return certError(c, err)
	}
	return c.Status(fiber.StatusAccepted).JSON(result)
}

func (h *CertificateHandler) ListTemplates(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	result, err := h.svc.ListTemplates(c.Context(), p)
	if err != nil {
		return certError(c, err)
	}
	return c.JSON(result)
}

func (h *CertificateHandler) GetTemplate(c fiber.Ctx) error {
	p, id, err := certificateTemplateCaller(c)
	if err != nil {
		return certError(c, err)
	}
	result, err := h.svc.GetTemplate(c.Context(), p, id)
	if err != nil {
		return certError(c, err)
	}
	return c.JSON(result)
}

func (h *CertificateHandler) CreateTemplate(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	var input certificate.TemplateDraft
	if err := c.Bind().Body(&input); err != nil {
		return certError(c, certificate.ErrInvalid)
	}
	result, err := h.svc.CreateTemplate(c.Context(), p, input)
	if err != nil {
		return certError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(result)
}

func (h *CertificateHandler) UpdateTemplate(c fiber.Ctx) error {
	p, id, err := certificateTemplateCaller(c)
	if err != nil {
		return certError(c, err)
	}
	var input certificate.TemplateDraft
	if err := c.Bind().Body(&input); err != nil {
		return certError(c, certificate.ErrInvalid)
	}
	result, err := h.svc.UpdateTemplate(c.Context(), p, id, input)
	if err != nil {
		return certError(c, err)
	}
	return c.JSON(result)
}

func (h *CertificateHandler) PublishTemplate(c fiber.Ctx) error {
	p, id, err := certificateTemplateCaller(c)
	if err != nil {
		return certError(c, err)
	}
	result, err := h.svc.PublishTemplate(c.Context(), p, id)
	if err != nil {
		return certError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(result)
}

func (h *CertificateHandler) PreviewTemplate(c fiber.Ctx) error {
	p, id, err := certificateTemplateCaller(c)
	if err != nil {
		return certError(c, err)
	}
	var sample certificate.PreviewData
	if c.Request().Header.ContentLength() > 0 {
		if err := c.Bind().Body(&sample); err != nil {
			return certError(c, certificate.ErrInvalid)
		}
	}
	pdf, err := h.svc.PreviewTemplate(c.Context(), p, id, sample)
	if err != nil {
		return certError(c, err)
	}
	c.Set(fiber.HeaderContentType, "application/pdf")
	return c.Send(pdf)
}

func (h *CertificateHandler) SetBinding(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	var input certificate.Binding
	if err := c.Bind().Body(&input); err != nil {
		return certError(c, certificate.ErrInvalid)
	}
	result, err := h.svc.SetBinding(c.Context(), p, input)
	if err != nil {
		return certError(c, err)
	}
	return c.JSON(result)
}

func (h *CertificateHandler) ListBindings(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	result, err := h.svc.ListBindings(c.Context(), p)
	if err != nil {
		return certError(c, err)
	}
	return c.JSON(result)
}

func (h *CertificateHandler) ClearBinding(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	if err := h.svc.ClearBinding(c.Context(), p, c.Params("scope"), c.Params("scopeKey")); err != nil {
		return certError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func certificateEventCaller(c fiber.Ctx) (authz.Principal, uuid.UUID, error) {
	p, err := caller(c)
	if err != nil {
		return authz.Principal{}, uuid.Nil, err
	}
	id, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return authz.Principal{}, uuid.Nil, certificate.ErrInvalid
	}
	return p, id, nil
}

func certificateTemplateCaller(c fiber.Ctx) (authz.Principal, uuid.UUID, error) {
	p, err := caller(c)
	if err != nil {
		return authz.Principal{}, uuid.Nil, err
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return authz.Principal{}, uuid.Nil, certificate.ErrInvalid
	}
	return p, id, nil
}
