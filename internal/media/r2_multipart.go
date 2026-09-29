package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// The R2 side of Direct upload (MultipartStore) and of a video's faststart
// copy (FaststartStorage). R2's S3 API supports every call here; R2 also
// requires every part but the last to be the same size, and at least 5 MiB,
// which the parts core hands out and writes are.

var (
	_ MultipartStore   = (*R2)(nil)
	_ FaststartStorage = (*R2)(nil)
)

func (r *R2) CreateMultipart(ctx context.Context, key string) (string, error) {
	// Completion copies the file to its final key with the serving policy's
	// metadata. Until then the joined object sits at its pending key, which
	// the public bucket's CDN would serve too: as an opaque download, never
	// rendered.
	return r.CreateMultipartWith(ctx, key, pendingMetadata)
}

// CreateMultipartWith opens a multipart upload at key whose object is
// stored with meta once completed.
func (r *R2) CreateMultipartWith(ctx context.Context, key string, meta BlobMetadata) (string, error) {
	out, err := r.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:             aws.String(r.bucket),
		Key:                aws.String(key),
		ContentType:        aws.String(meta.ContentType),
		ContentDisposition: stringOrNil(meta.ContentDisposition),
	})
	if err != nil {
		return "", err
	}
	if out.UploadId == nil || *out.UploadId == "" {
		return "", errors.New("media: storage opened a multipart upload without an id")
	}
	return *out.UploadId, nil
}

// PresignPart signs the part's length with it (Content-Length is a signed
// header), so the browser can send exactly size bytes to it and no more.
// The address carries the signature; it is handed to the uploader and never
// logged.
func (r *R2) PresignPart(ctx context.Context, key, uploadID string, number int32, size int64, ttl time.Duration) (string, error) {
	presigned, err := s3.NewPresignClient(r.client).PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket:        aws.String(r.bucket),
		Key:           aws.String(key),
		UploadId:      aws.String(uploadID),
		PartNumber:    aws.Int32(number),
		ContentLength: aws.Int64(size),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return presigned.URL, nil
}

// PresignGet is a GET of the object at key that works without credentials
// for ttl: the frame service reads a video by it (ranged GETs). The address
// carries the signature; it is handed to the frame service and never
// logged.
func (r *R2) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	presigned, err := s3.NewPresignClient(r.client).PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return presigned.URL, nil
}

// UploadPart sends one part's bytes from core itself (a browser sends its
// parts to presigned addresses instead); ErrMultipartGone when the upload
// is.
func (r *R2) UploadPart(ctx context.Context, key, uploadID string, number int32, data []byte) (string, error) {
	out, err := r.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:        aws.String(r.bucket),
		Key:           aws.String(key),
		UploadId:      aws.String(uploadID),
		PartNumber:    aws.Int32(number),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
	})
	if apiErrorCode(err) == "NoSuchUpload" {
		return "", ErrMultipartGone
	}
	if err != nil {
		return "", err
	}
	if aws.ToString(out.ETag) == "" {
		return "", errors.New("media: storage stored a part without an ETag")
	}
	return aws.ToString(out.ETag), nil
}

// UploadPartCopy has storage copy the n bytes of the object at from, from
// off, as one part: they never leave the bucket. ErrNotFound when there is
// no such object, ErrMultipartGone when the upload is gone.
func (r *R2) UploadPartCopy(ctx context.Context, key, uploadID string, number int32, from string, off, n int64) (string, error) {
	if off < 0 || n <= 0 {
		return "", ErrInvalid
	}
	out, err := r.client.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
		Bucket:          aws.String(r.bucket),
		Key:             aws.String(key),
		UploadId:        aws.String(uploadID),
		PartNumber:      aws.Int32(number),
		CopySource:      aws.String((&url.URL{Path: r.bucket + "/" + from}).EscapedPath()),
		CopySourceRange: aws.String(fmt.Sprintf("bytes=%d-%d", off, off+n-1)),
	})
	switch apiErrorCode(err) {
	case "NoSuchKey":
		return "", ErrNotFound
	case "NoSuchUpload":
		return "", ErrMultipartGone
	}
	if err != nil {
		return "", err
	}
	if out.CopyPartResult == nil || aws.ToString(out.CopyPartResult.ETag) == "" {
		return "", errors.New("media: storage copied a part without an ETag")
	}
	return aws.ToString(out.CopyPartResult.ETag), nil
}

// ListKeys lists the keys under prefix (ListObjectsV2), every page of them.
func (r *R2) ListKeys(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	var token *string
	for {
		out, err := r.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(r.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, o := range out.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
		if !aws.ToBool(out.IsTruncated) || out.NextContinuationToken == nil {
			return keys, nil
		}
		token = out.NextContinuationToken
	}
}

func (r *R2) ListParts(ctx context.Context, key, uploadID string) ([]UploadedPart, error) {
	var parts []UploadedPart
	var marker *string
	for {
		out, err := r.client.ListParts(ctx, &s3.ListPartsInput{
			Bucket:           aws.String(r.bucket),
			Key:              aws.String(key),
			UploadId:         aws.String(uploadID),
			PartNumberMarker: marker,
		})
		if apiErrorCode(err) == "NoSuchUpload" {
			return nil, ErrMultipartGone
		}
		if err != nil {
			return nil, err
		}
		for _, p := range out.Parts {
			parts = append(parts, UploadedPart{Number: aws.ToInt32(p.PartNumber), ETag: aws.ToString(p.ETag), Size: aws.ToInt64(p.Size)})
		}
		if !aws.ToBool(out.IsTruncated) || out.NextPartNumberMarker == nil {
			return parts, nil
		}
		marker = out.NextPartNumberMarker
	}
}

func (r *R2) CompleteMultipart(ctx context.Context, key, uploadID string, parts []UploadedPart) error {
	completed := make([]types.CompletedPart, 0, len(parts))
	for _, p := range parts {
		completed = append(completed, types.CompletedPart{PartNumber: aws.Int32(p.Number), ETag: aws.String(p.ETag)})
	}
	_, err := r.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(r.bucket),
		Key:             aws.String(key),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	switch apiErrorCode(err) {
	case "NoSuchUpload":
		return ErrMultipartGone
	case "InvalidPart", "InvalidPartOrder", "EntityTooSmall":
		return fmt.Errorf("%w: %s", ErrMultipartPartsMismatch, apiErrorCode(err))
	}
	return err
}

func (r *R2) Size(ctx context.Context, key string) (int64, error) {
	out, err := r.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
	})
	if code := apiErrorCode(err); code == "NotFound" || code == "NoSuchKey" {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return aws.ToInt64(out.ContentLength), nil
}

// ReadStart asks only for the bytes it reads (a ranged GET).
func (r *R2) ReadStart(ctx context.Context, key string, n int) ([]byte, error) {
	if n <= 0 {
		return nil, ErrInvalid
	}
	got, err := r.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
		Range:  aws.String(fmt.Sprintf("bytes=0-%d", n-1)),
	})
	switch apiErrorCode(err) {
	case "NoSuchKey":
		return nil, ErrNotFound
	case "InvalidRange":
		// An empty object has no first byte to start a range at.
		return []byte{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer got.Body.Close()
	return io.ReadAll(io.LimitReader(got.Body, int64(n)))
}

// OpenRange streams the n bytes of the object from off (a ranged GET);
// ErrNotFound when there is none. Storage must answer exactly that range:
// an answer of other bytes (the whole object, a shorter range) is an error,
// never read as the range.
func (r *R2) OpenRange(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	if off < 0 || n <= 0 {
		return nil, ErrInvalid
	}
	last := off + n - 1
	got, err := r.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", off, last)),
	})
	if apiErrorCode(err) == "NoSuchKey" {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	answered := aws.ToString(got.ContentRange)
	if !strings.HasPrefix(answered, fmt.Sprintf("bytes %d-%d/", off, last)) || aws.ToInt64(got.ContentLength) != n {
		got.Body.Close()
		return nil, fmt.Errorf("r2: asked for bytes %d-%d, answered %q", off, last, answered)
	}
	return got.Body, nil
}

func (r *R2) Copy(ctx context.Context, from, to string, meta BlobMetadata) error {
	_, err := r.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:             aws.String(r.bucket),
		Key:                aws.String(to),
		CopySource:         aws.String((&url.URL{Path: r.bucket + "/" + from}).EscapedPath()),
		MetadataDirective:  types.MetadataDirectiveReplace,
		ContentType:        aws.String(meta.ContentType),
		ContentDisposition: stringOrNil(meta.ContentDisposition),
	})
	if apiErrorCode(err) == "NoSuchKey" {
		return ErrNotFound
	}
	return err
}

// abortMultipartUploads aborts every multipart upload open at exactly key.
// One already gone counts as aborted.
func (r *R2) abortMultipartUploads(ctx context.Context, key string) error {
	var keyMarker, idMarker *string
	for {
		out, err := r.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
			Bucket:         aws.String(r.bucket),
			Prefix:         aws.String(key),
			KeyMarker:      keyMarker,
			UploadIdMarker: idMarker,
		})
		if err != nil {
			return err
		}
		for _, upload := range out.Uploads {
			if aws.ToString(upload.Key) != key {
				continue
			}
			_, err := r.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
				Bucket:   aws.String(r.bucket),
				Key:      aws.String(key),
				UploadId: upload.UploadId,
			})
			if err != nil && apiErrorCode(err) != "NoSuchUpload" {
				return err
			}
		}
		if !aws.ToBool(out.IsTruncated) || (out.NextKeyMarker == nil && out.NextUploadIdMarker == nil) {
			return nil
		}
		keyMarker, idMarker = out.NextKeyMarker, out.NextUploadIdMarker
	}
}

// apiErrorCode is the S3 error code of err; "" for no error or another
// kind of failure.
func apiErrorCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}
