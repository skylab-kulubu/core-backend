package clamd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
)

// eicarParts are the EICAR anti-malware test file in two halves. Every
// scanner reports the file and it harms nothing; it is put together only at
// run time (EICAR) so that core's source and binary never carry it whole,
// which a scanner on a developer's machine or in a registry would flag.
var eicarParts = []string{`X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR`, `-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`}

// EICAR is the EICAR test file.
func EICAR() []byte {
	return []byte(strings.Join(eicarParts, ""))
}

// ErrSelfTestMissed is a clamd that answered the EICAR test file clean: it
// runs without a signature database that knows it, and would pass anything.
var ErrSelfTestMissed = errors.New("clamd: the EICAR test file was answered clean")

// SelfTestResult is what the self-test saw: clamd's version (with its
// database's) and the name it gave the test file.
type SelfTestResult struct {
	Version   string
	Signature string
}

// SelfTest sends the EICAR test file to clamd and passes only when clamd
// reports it (FOUND). The deploy check runs it inside core's container
// (core-backend media-scan-selftest), so it also proves core reaches clamd.
func SelfTest(ctx context.Context, c *Client) (SelfTestResult, error) {
	version, err := c.Version(ctx)
	if err != nil {
		return SelfTestResult{}, err
	}
	result, err := c.Scan(ctx, bytes.NewReader(EICAR()))
	if err != nil {
		return SelfTestResult{Version: version}, err
	}
	if !result.Infected() {
		return SelfTestResult{Version: version}, fmt.Errorf("%w (%s)", ErrSelfTestMissed, version)
	}
	return SelfTestResult{Version: version, Signature: result.Signature}, nil
}
