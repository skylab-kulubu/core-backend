package qr

import (
	"os"
	"strconv"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

const DefaultSize = 256

func PNG(content string, size int) ([]byte, error) {
	if size < 64 || size > 1024 {
		size = DefaultSize
	}
	return qrcode.Encode(content, qrcode.Medium, size)
}

func SizeFromQuery(raw string) int {
	if raw == "" {
		return DefaultSize
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return DefaultSize
	}
	return n
}

func ShortURL(alias string) string {
	origin := strings.TrimRight(os.Getenv("SHORT_ORIGIN"), "/")
	if origin == "" {
		origin = "https://skyl.app"
	}
	return origin + "/" + alias
}
