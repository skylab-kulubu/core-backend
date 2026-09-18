package media_test

import (
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/media"
)

func TestPublicURLJoinsStorageKeys(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		base string
		key  string
		want string
	}{
		{name: "key", key: "images/b0db8eb9-5914-4db7-a39a-cabd6bf47faa", want: "https://cdn.yildizskylab.com/images/b0db8eb9-5914-4db7-a39a-cabd6bf47faa"},
		{name: "leading slash", key: "/images/7f863471-c2b6-45f4-912d-80f97daf25f4", want: "https://cdn.yildizskylab.com/images/7f863471-c2b6-45f4-912d-80f97daf25f4"},
		{name: "already https", key: "https://cdn.yildizskylab.com/images/abc", want: "https://cdn.yildizskylab.com/images/abc"},
		{name: "other host", key: "https://other.example/file.jpg", want: "https://other.example/file.jpg"},
		{name: "empty", key: "", want: ""},
		{name: "whitespace", key: "   ", want: ""},
		{name: "custom base", base: "https://cdn.example.test/", key: "images/x", want: "https://cdn.example.test/images/x"},
		{name: "duplicate slashes", base: "https://cdn.yildizskylab.com/", key: "/images/x", want: "https://cdn.yildizskylab.com/images/x"},
		{name: "http left alone", key: "http://cdn.example.test/images/x", want: "http://cdn.example.test/images/x"},
		{name: "files key", key: "files/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", want: "https://cdn.yildizskylab.com/files/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := media.PublicURL(tc.base, tc.key)
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
			if got != "" && strings.Contains(got, "/events/") {
				t.Fatalf("joined against app origin %s", got)
			}
		})
	}
}
