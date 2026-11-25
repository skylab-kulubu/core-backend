package certificate

// InvalidError is an ErrInvalid that says why, as the problem code the API
// answers (docs/certificate-template-errors.md): the panel tells the
// designer what to change instead of a bare 400.
type InvalidError struct{ Code string }

func (e *InvalidError) Error() string { return "certificate: invalid: " + e.Code }

func (e *InvalidError) Is(target error) bool { return target == ErrInvalid }

func invalid(code string) error { return &InvalidError{Code: code} }

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
)
