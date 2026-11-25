package main

import (
	"log"

	"github.com/skylab-kulubu/core-backend/internal/certificate"
)

// certificatePageOrigin is CERTIFICATE_PUBLIC_PAGE_ORIGIN, the certificate
// page on the club's site: with it, pdfUrl opens the PDF there
// (docs/certificate-pdf-address.md). A value that is not an http(s) address
// stops core at startup.
func certificatePageOrigin(getenv func(string) string) string {
	origin, err := certificate.PublicPageOrigin(getenv("CERTIFICATE_PUBLIC_PAGE_ORIGIN"))
	if err != nil {
		log.Fatal(err)
	}
	return origin
}
