package authz

import "testing"

// A certificate font is uploaded by who may create a certificate template
// for some team, or a club-wide one: the template rule alone. An Event's
// editor who may not create a template makes its Event template and
// uploads its background, but not a font (docs/certificate-fonts.md).
func TestAuthorizer_CertificateFontUploadIsTheTemplateRule(t *testing.T) {
	t.Parallel()
	auth := NewAuthorizer(DefaultPolicy())
	font := Resource{Type: TypeMedia, MediaUploader: MediaUploaderCertificateFontEditor}
	asset := Resource{Type: TypeMedia, MediaUploader: MediaUploaderCertificateTemplateEditor}
	cases := []struct {
		name        string
		p           Principal
		font, asset bool
	}{
		{"GECEKODU member (edits its Events)", Principal{ID: "u1", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}}, false, true},
		{"WEBLAB Leader", Principal{ID: "u2", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}, true, true},
		{"WEBLAB member", Principal{ID: "u3", Groups: []string{"/UYELER/ARGE/WEBLAB"}}, false, false},
		{"WEBLAB member with certificate:template:manage", Principal{ID: "u4", Groups: []string{"/UYELER/ARGE/WEBLAB"}, Roles: []string{"certificate:template:manage"}}, true, true},
		{"ADMIN", Principal{ID: "u5", Groups: []string{"/ADMIN"}}, true, true},
		{"YK", Principal{ID: "u8", Groups: []string{"/YK"}}, true, true},
		{"DK (reads only)", Principal{ID: "u6", Groups: []string{"/DK"}}, false, false},
		{"nobody's member", Principal{ID: "u7"}, false, false},
	}
	for _, c := range cases {
		if got := auth.Allow(c.p, font, Upload); got != c.font {
			t.Errorf("%s: font upload %v, want %v", c.name, got, c.font)
		}
		if got := auth.Allow(c.p, asset, Upload); got != c.asset {
			t.Errorf("%s: asset upload %v, want %v", c.name, got, c.asset)
		}
	}
	if !MediaUploaderCertificateFontEditor.Known() {
		t.Error("the catalogue may not name certificate_font_editor")
	}
	// In the roles mode, the template role alone.
	roles := NewAuthorizer(policyIn(RoleModeRoles))
	if !roles.Allow(Principal{ID: "u1", Roles: []string{RoleCertificateManage}}, font, Upload) {
		t.Errorf("%s holder refused a font upload", RoleCertificateManage)
	}
}
