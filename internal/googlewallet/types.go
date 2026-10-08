package googlewallet

// The Generic pass resources core writes, with only the fields it uses
// (https://developers.google.com/wallet/generic/rest/v1/genericclass,
// https://developers.google.com/wallet/generic/rest/v1/genericobject).

const (
	StateActive   = "ACTIVE"
	StateInactive = "INACTIVE"

	// OneUserAllDevices: once one Google account saved the object, no
	// other account can view or save it; that account sees it on all of
	// its devices.
	OneUserAllDevices = "ONE_USER_ALL_DEVICES"
	// UnlockRequiredToView: the device must be unlocked each time the
	// pass is opened.
	UnlockRequiredToView = "UNLOCK_REQUIRED_TO_VIEW"
	// ScreenshotIneligible blocks screenshots of the pass on Android (older
	// Wallet versions may still allow them).
	ScreenshotIneligible = "INELIGIBLE"

	GenericTypeOther = "GENERIC_OTHER"
	BarcodeQRCode    = "QR_CODE"
	TOTPSHA1         = "TOTP_SHA1"
)

type GenericClass struct {
	ID                                     string `json:"id"`
	MultipleDevicesAndHoldersAllowedStatus string `json:"multipleDevicesAndHoldersAllowedStatus,omitempty"`
	ViewUnlockRequirement                  string `json:"viewUnlockRequirement,omitempty"`
}

type GenericObject struct {
	ID              string           `json:"id"`
	ClassID         string           `json:"classId"`
	GenericType     string           `json:"genericType,omitempty"`
	State           string           `json:"state,omitempty"`
	CardTitle       *LocalizedString `json:"cardTitle,omitempty"`
	Subheader       *LocalizedString `json:"subheader,omitempty"`
	Header          *LocalizedString `json:"header,omitempty"`
	Logo            *Image           `json:"logo,omitempty"`
	TextModulesData []TextModuleData `json:"textModulesData,omitempty"`
	RotatingBarcode *RotatingBarcode `json:"rotatingBarcode,omitempty"`
	PassConstraints *PassConstraints `json:"passConstraints,omitempty"`
}

type LocalizedString struct {
	DefaultValue TranslatedString `json:"defaultValue"`
}

type TranslatedString struct {
	Language string `json:"language"`
	Value    string `json:"value"`
}

// Localized is a LocalizedString with one value.
func Localized(language, value string) *LocalizedString {
	return &LocalizedString{DefaultValue: TranslatedString{Language: language, Value: value}}
}

type Image struct {
	SourceURI ImageURI `json:"sourceUri"`
}

type ImageURI struct {
	URI string `json:"uri"`
}

type TextModuleData struct {
	ID     string `json:"id,omitempty"`
	Header string `json:"header,omitempty"`
	Body   string `json:"body,omitempty"`
}

// RotatingBarcode is a barcode the device redraws on its own: valuePattern
// with {totp_value_0} replaced by the RFC 6238 code of TotpDetails.
type RotatingBarcode struct {
	Type          string       `json:"type"`
	ValuePattern  string       `json:"valuePattern"`
	AlternateText string       `json:"alternateText,omitempty"`
	TotpDetails   *TotpDetails `json:"totpDetails,omitempty"`
}

type TotpDetails struct {
	// PeriodMillis is an int64, which the API takes as a string.
	PeriodMillis string           `json:"periodMillis"`
	Algorithm    string           `json:"algorithm"`
	Parameters   []TotpParameters `json:"parameters"`
}

type TotpParameters struct {
	// Key is the TOTP secret, Base16.
	Key         string `json:"key"`
	ValueLength int    `json:"valueLength"`
}

type PassConstraints struct {
	ScreenshotEligibility string `json:"screenshotEligibility,omitempty"`
}
