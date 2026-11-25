package certificate

import (
	"fmt"
	"net/url"
	"strings"
)

// PublicPageOrigin reads CERTIFICATE_PUBLIC_PAGE_ORIGIN, the certificate
// page's address on the club's site (Options.PublicPageOrigin,
// docs/certificate-pdf-address.md): an absolute http(s) address with a host
// and nothing after its path, returned without its trailing slash. Empty
// (unset) answers "" with no error; anything else that is not such an
// address is an error, so a typo stops core at startup instead of handing
// out broken links.
func PublicPageOrigin(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Contains(value, "#") {
		return "", fmt.Errorf("CERTIFICATE_PUBLIC_PAGE_ORIGIN must be the certificate page's http(s) address, e.g. https://yildizskylab.com/sertifika")
	}
	return strings.TrimRight(value, "/"), nil
}
