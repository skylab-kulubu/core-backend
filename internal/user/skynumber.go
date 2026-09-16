package user

import (
	"fmt"
	"strconv"
	"strings"
)

const skyPrefix = "SKY-"

func FormatSkyNumber(n int) (string, error) {
	if n < 1 || n > 9999999 {
		return "", ErrSkyLimit
	}
	return fmt.Sprintf("SKY-%07d", n), nil
}

func parseSkyNumber(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, skyPrefix) {
		return 0, false
	}
	n, err := strconv.Atoi(s[len(skyPrefix):])
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}
