package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type R2Config struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
}

// Read returns an object's bytes; ErrNotFound when there is no such object.
func (r *R2) Read(ctx context.Context, key string) ([]byte, error) {
	got, err := r.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
	})
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchKey" {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer got.Body.Close()
	return io.ReadAll(got.Body)
}

// Open streams a stored object; ErrNotFound when there is none. The caller
// closes it.
func (r *R2) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	got, err := r.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
	})
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchKey" {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return got.Body, nil
}

type R2 struct {
	client *s3.Client
	bucket string
	// cdn queues the CDN address of each object deleted or given new
	// metadata (PurgeCDNOnChange); nil for a bucket the CDN does not
	// serve.
	cdn CDNKeyQueue
}

// PurgeCDNOnChange has every object this bucket deletes, or whose metadata
// it replaces, queued for a CDN cache purge once storage has done it (media
// redesign ticket 29). Only the public bucket, which the CDN serves, is
// given one; core sets it at startup, before the bucket is used.
func (r *R2) PurgeCDNOnChange(queue CDNKeyQueue) {
	r.cdn = queue
}

// changed queues key's CDN purge. Its error fails the call that changed the
// object, so the caller does it again (every delete by key and every
// metadata rewrite may be repeated) and the purge is queued again.
func (r *R2) changed(ctx context.Context, key string) error {
	if r.cdn == nil {
		return nil
	}
	return r.cdn.QueueKey(ctx, key)
}

func (r *R2) Bucket() string {
	return r.bucket
}

func NewR2(cfg R2Config) *R2 {
	client := s3.New(s3.Options{
		BaseEndpoint:               aws.String(cfg.Endpoint),
		Region:                     "auto",
		Credentials:                credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		UsePathStyle:               true,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &R2{client: client, bucket: cfg.Bucket}
}

func (r *R2) Put(ctx context.Context, key string, data []byte, meta BlobMetadata) error {
	_, err := r.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:             aws.String(r.bucket),
		Key:                aws.String(key),
		Body:               bytes.NewReader(data),
		ContentType:        aws.String(meta.ContentType),
		ContentDisposition: stringOrNil(meta.ContentDisposition),
		ContentLength:      aws.Int64(int64(len(data))),
	})
	if err != nil {
		return err
	}
	_, err = r.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
	})
	return err
}

// SetMetadata replaces the serving metadata of a stored object without
// rewriting its bytes: a copy onto the same key with the REPLACE directive,
// which R2's S3 API supports for CopyObject.
func (r *R2) SetMetadata(ctx context.Context, key string, meta BlobMetadata) error {
	_, err := r.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:             aws.String(r.bucket),
		Key:                aws.String(key),
		CopySource:         aws.String((&url.URL{Path: r.bucket + "/" + key}).EscapedPath()),
		MetadataDirective:  types.MetadataDirectiveReplace,
		ContentType:        aws.String(meta.ContentType),
		ContentDisposition: stringOrNil(meta.ContentDisposition),
	})
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchKey" {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return r.changed(ctx, key)
}

// Delete removes a stored object. An object that is not there is deleted
// already: S3 answers that with success, and an endpoint that answers
// NoSuchKey instead is taken the same way, so every delete by key (purges,
// account erasure) can be repeated.
//
// A pending key (isPendingKey) is a Direct upload's, and a faststart key
// (isFaststartKey) a video's rewrite: deleting one first aborts any
// multipart upload still open at it, so every path that deletes by key
// alone (the staging sweeper, account erasure, a refused completion, a
// purge, a rewrite cut short) leaves no parts behind either.
//
// An object deleted, or not there, is queued for a CDN purge
// (PurgeCDNOnChange): the CDN may still hold a copy of it.
func (r *R2) Delete(ctx context.Context, key string) error {
	if isPendingKey(key) || isFaststartKey(key) {
		if err := r.abortMultipartUploads(ctx, key); err != nil {
			return err
		}
	}
	_, err := r.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
	})
	var apiErr smithy.APIError
	if err != nil && !(errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchKey") {
		return err
	}
	return r.changed(ctx, key)
}

// stringOrNil leaves an empty header unset instead of sending it empty.
func stringOrNil(value string) *string {
	if value == "" {
		return nil
	}
	return aws.String(value)
}
