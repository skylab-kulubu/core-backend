package certificate

import "context"

// recordingPDFTools (pdf_placement_test.go) reads no text: the placement
// tests never ask it to.
func (r *recordingPDFTools) Text(context.Context, []byte) (PDFText, error) {
	return PDFText{Info: r.info}, r.err
}

func (r *recordingPDFTools) StripText(context.Context, []byte, []string) ([]byte, error) {
	return nil, r.err
}
