package media

import "strings"

// SizeObject is one of an image's sizes stored as its own object beside
// it: how large it is, and its type (JPEG or PNG), which names its key.
type SizeObject struct {
	ImageSize
	Type string `json:"type"`
}

// sizeObjectTypes are the types a size object is encoded as.
var sizeObjectTypes = []string{"image/jpeg", "image/png"}

// The one rule for where an image's sizes are stored: upload, the size
// backfill and every purge ask here.

// canHaveSizeObjects reports whether sizes may be stored beside the object
// at key: only an object core itself stored under images/. The size
// backfill leaves any other key (an absolute address kept from before core
// stored keys, a key of another shape) without sizes.
func canHaveSizeObjects(key string) bool {
	return strings.HasPrefix(key, "images/") && !isAbsoluteURL(key)
}

// sizeObjectKey is the object key of an image's size: next to the image's
// own key, with the extension of its type, so a CDN that caches by
// extension caches it.
func sizeObjectKey(key, size, ctype string) string {
	extension := ".png"
	if ctype == "image/jpeg" {
		extension = ".jpg"
	}
	return key + "/" + size + extension
}

// sizeObjectKeys are every key a size of the object at key may be stored
// at, whatever its record says: an upload or a backfill cut short may have
// written a size the record never got.
func sizeObjectKeys(key string) []string {
	if !canHaveSizeObjects(key) {
		return nil
	}
	var keys []string
	for _, size := range imageSizes {
		for _, ctype := range sizeObjectTypes {
			keys = append(keys, sizeObjectKey(key, size, ctype))
		}
	}
	return keys
}

// purgeObjects deletes every object a Media key may have: the object
// itself, then its sizes. A failure part way leaves the purge claimed, and
// its retry deletes them all again (deleting an object that is not there
// succeeds), so the order only decides what a crash leaves until then: a
// size, never the image without its purge recorded.
func purgeObjects(key string, purge func(key string) error) error {
	if err := purge(key); err != nil {
		return err
	}
	for _, sizeKey := range sizeObjectKeys(key) {
		if err := purge(sizeKey); err != nil {
			return err
		}
	}
	return nil
}
