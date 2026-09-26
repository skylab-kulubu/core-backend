package media

import "strings"

// sizeObjectKey is the object key of an image's stored size: next to the
// image's own key, so every cleanup that knows the key finds its sizes.
func sizeObjectKey(key, size string) string {
	return key + "/" + size
}

// purgeObjects deletes every object a Media key may have: the stored sizes
// of an image, then the object itself, so a failure part way leaves the
// object for the retry to find. The sizes are derived from the key, not read
// from the record: an upload or a backfill cut short may have written a size
// the record never got. Deleting an object that is not there succeeds.
func purgeObjects(key string, purge func(key string) error) error {
	if strings.HasPrefix(key, "images/") {
		for _, size := range imageSizes {
			if err := purge(sizeObjectKey(key, size)); err != nil {
				return err
			}
		}
	}
	return purge(key)
}
