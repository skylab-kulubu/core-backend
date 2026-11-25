package certificate

// InvalidError is an ErrInvalid that says why, as the problem code the API
// answers (docs/certificate-template-errors.md): the panel tells the
// designer what to change instead of a bare 400.
type InvalidError struct{ Code string }

func (e *InvalidError) Error() string { return "certificate: invalid: " + e.Code }

func (e *InvalidError) Is(target error) bool { return target == ErrInvalid }

func invalid(code string) error { return &InvalidError{Code: code} }

// UnavailableError is a renderer that could not be reached, is not set up
// or failed on its side (a 5xx): a later try may pass. It is not
// ErrInvalid, so an issuance job tries it again; the API answers 503 with
// its code. Err is the cause, for the log.
type UnavailableError struct {
	Code string
	Err  error
}

func (e *UnavailableError) Error() string {
	if e.Err == nil {
		return "certificate: unavailable: " + e.Code
	}
	return "certificate: unavailable: " + e.Code + ": " + e.Err.Error()
}

func (e *UnavailableError) Unwrap() error { return e.Err }

// rendererUnavailable is the renderer down or missing, with its cause.
func rendererUnavailable(cause error) error {
	return &UnavailableError{Code: CodeRendererUnavailable, Err: cause}
}

// The codes of a template's layout, its assets and its rendering.
const (
	CodeTemplateNameInvalid     = "template_name_invalid"
	CodeTemplateSourceInvalid   = "template_source_invalid"
	CodeLayoutSizeInvalid       = "layout_size_invalid"
	CodeLayoutOrientation       = "layout_orientation_invalid"
	CodeLayoutColorInvalid      = "layout_color_invalid"
	CodeLayoutElementCount      = "layout_element_count"
	CodeLayoutElementID         = "layout_element_id_invalid"
	CodeLayoutElementKind       = "layout_element_kind_unknown"
	CodeLayoutOutOfBounds       = "layout_element_out_of_bounds"
	CodeLayoutElementStyle      = "layout_element_style_invalid"
	CodeLayoutTextTooLong       = "layout_text_too_long"
	CodeLayoutImageMissing      = "layout_image_missing"
	CodeLayoutRecipientMissing  = "layout_recipient_name_missing"
	CodeLayoutNotVerifiable     = "layout_not_verifiable"
	CodeAssetEmpty              = "asset_empty"
	CodeAssetTooLarge           = "asset_too_large"
	CodeAssetTypeUnsupported    = "asset_type_unsupported"
	CodeAssetStorageUnavailable = "asset_storage_unavailable"
	CodeBackgroundPDFUnreadable = "background_pdf_unreadable"
	CodeBackgroundPDFTooLarge   = "background_pdf_too_large"
	CodeBackgroundPDFPages      = "background_pdf_pages"
	CodeRendererUnavailable     = "renderer_unavailable"
	CodeRenderFailed            = "render_failed"
	// CodeTemplateEventScoped is an Event template bound to another scope
	// or another Event (400); CodeTemplateEventExists a second Event
	// template for an Event (409, EventTemplateExistsError).
	CodeTemplateEventScoped = "template_event_scoped"
	CodeTemplateEventExists = "template_event_exists"
)
