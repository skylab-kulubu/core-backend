package shorturl

import "testing"

func TestInferSourceReadsInAppBrowsersAndReferrers(t *testing.T) {
	t.Parallel()
	const instagramApp = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 Instagram 312.0.0.24.108 (iPhone13,2; iOS 17_0; tr_TR)"
	const linkedinApp = "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Mobile Safari/537.36 [LinkedInApp]"
	const browser = "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Mobile Safari/537.36"
	cases := []struct {
		name, userAgent, referer, want string
	}{
		{"instagram in-app browser", instagramApp, "", "instagram"},
		{"instagram link shim", browser, "https://l.instagram.com/", "instagram"},
		{"linkedin in-app browser", linkedinApp, "", "linkedin"},
		{"linkedin short link", browser, "https://lnkd.in/abc", "linkedin"},
		{"youtube description", browser, "https://www.youtube.com/", "youtube"},
		{"youtube mobile", browser, "https://m.youtube.com/watch?v=x", "youtube"},
		{"youtu.be", browser, "https://youtu.be/x", "youtube"},
		{"whatsapp leaves no trace", browser, "", ""},
		{"look-alike host", browser, "https://notyoutube.com/", ""},
		{"broken referer", browser, "://", ""},
	}
	for _, c := range cases {
		if got := InferSource(c.userAgent, c.referer); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestWithInferredSourceKeepsATag(t *testing.T) {
	t.Parallel()
	const instagramApp = "Mozilla/5.0 Mobile/15E148 Instagram 312.0.0.24.108"
	if got := (UTM{Source: "whatsapp"}).WithInferredSource(instagramApp, ""); got.Source != "whatsapp" || got.Medium != "" {
		t.Fatalf("tagged hop %+v", got)
	}
	if got := (UTM{}).WithInferredSource(instagramApp, ""); got.Source != "instagram" || got.Medium != MediumReferral {
		t.Fatalf("untagged hop %+v", got)
	}
	if got := (UTM{Medium: "story"}).WithInferredSource(instagramApp, ""); got.Source != "instagram" || got.Medium != "story" {
		t.Fatalf("a medium the link carries stays %+v", got)
	}
	if got := (UTM{Campaign: "guz"}).WithInferredSource("curl/8", ""); got.Source != "" || got.Medium != "" || got.Campaign != "guz" {
		t.Fatalf("unknown hop %+v", got)
	}
}
