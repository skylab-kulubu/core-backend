package media

// One ceiling for every animation, GIF or WebP.
const (
	// maxAnimationFrames is the most frames core keeps in an animation.
	maxAnimationFrames = 300
	// maxAnimationPixels is the most pixels an animation may show across
	// its frames, counted as frames × canvas (a GIF's logical screen): a
	// viewer composes every frame over the whole canvas.
	maxAnimationPixels = 256 << 20
)

// checkAnimation refuses an animation beyond the ceiling: more than
// maxAnimationFrames frames is ErrInvalid, more than maxAnimationPixels
// frames × canvas pixels errImageTooLarge with the most canvas pixels
// that many frames may have.
func checkAnimation(frames int, canvas ImageSize) error {
	if frames > maxAnimationFrames {
		return ErrInvalid
	}
	if int64(frames)*int64(canvas.Width)*int64(canvas.Height) > maxAnimationPixels {
		return errImageTooLarge{maxPixels: maxAnimationPixels / int64(frames)}
	}
	return nil
}
