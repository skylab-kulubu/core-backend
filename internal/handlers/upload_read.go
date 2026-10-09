package handlers

import (
	"errors"
	"io"
)

// errUploadSize is an uploaded file whose content is not the size its
// multipart header recorded.
var errUploadSize = errors.New("handlers: the uploaded file is not the size its form recorded")

// readUploadedFile reads an uploaded file of size bytes (its multipart
// header's Size) into one buffer of that size. io.ReadAll grows its buffer
// as it reads: for a 50 MiB Answer file it allocates several times the file
// and holds two copies at its last step, which counts against core's
// memory limit (docs/memory-limit.md).
func readUploadedFile(f io.Reader, size int64) ([]byte, error) {
	if size < 0 {
		return nil, errUploadSize
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(f, data); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return nil, errUploadSize
		}
		return nil, err
	}
	var extra [1]byte
	if n, _ := f.Read(extra[:]); n > 0 {
		return nil, errUploadSize
	}
	return data, nil
}
