package authz

import "testing"

// A certificate asset is uploaded by who may create a certificate template
// or edit an Event: a GECEKODU member edits GECEKODU's Events, so they make
// its Event template and upload its background
// (docs/certificate-event-templates.md). The certificate template decision
// itself stays narrow: the member still may not create a team template.
func TestAuthorizer_CertificateAssetUploadIncludesEventEditors(t *testing.T) {
	t.Parallel()
	auth := NewAuthorizer(DefaultPolicy())
	upload := Resource{Type: TypeMedia, MediaUploader: MediaUploaderCertificateTemplateEditor}
	cases := []struct {
		name string
		p    Principal
		want bool
	}{
		{"GECEKODU member (edits its Events)", Principal{ID: "u1", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}}, true},
		{"WEBLAB Leader", Principal{ID: "u2", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}, true},
		{"WEBLAB member", Principal{ID: "u3", Groups: []string{"/UYELER/ARGE/WEBLAB"}}, false},
		{"WEBLAB member with certificate:template:manage", Principal{ID: "u4", Groups: []string{"/UYELER/ARGE/WEBLAB"}, Roles: []string{"certificate:template:manage"}}, true},
		{"ADMIN", Principal{ID: "u5", Groups: []string{"/ADMIN"}}, true},
		{"DK (reads only)", Principal{ID: "u6", Groups: []string{"/DK"}}, false},
		{"nobody's member", Principal{ID: "u7"}, false},
	}
	for _, c := range cases {
		if got := auth.Allow(c.p, upload, Upload); got != c.want {
			t.Errorf("%s: upload %v, want %v", c.name, got, c.want)
		}
	}
	member := Principal{ID: "u1", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}}
	if auth.Allow(member, Resource{Type: TypeCertificateTemplate, OwnerTeam: "GECEKODU"}, Create) {
		t.Error("a GECEKODU member may create a team template")
	}
}
