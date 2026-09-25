package media

import "context"

// imageWork bounds how many images core decodes and encodes at once, so
// that concurrent uploads and the variant backfill together hold at most
// this many decoded images in memory (each at most maxDecodedImageBytes).
var imageWork = make(chan struct{}, 2)

// acquireImageWork waits for a turn to decode and encode an image. The
// returned release ends the turn.
func acquireImageWork(ctx context.Context) (release func(), err error) {
	select {
	case imageWork <- struct{}{}:
		return func() { <-imageWork }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
