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

func (r *R2) Read(ctx context.Context, key string) ([]byte, error) {
	got, err := r.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer got.Body.Close()
	return io.ReadAll(got.Body)
}

type R2 struct {
	client *s3.Client
	bucket string
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
		ContentDisposition: optional(meta.ContentDisposition),
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
		ContentDisposition: optional(meta.ContentDisposition),
	})
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchKey" {
		return ErrNotFound
	}
	return err
}

func (r *R2) Delete(ctx context.Context, key string) error {
	_, err := r.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
	})
	return err
}

// optional leaves an empty header unset instead of sending it empty.
func optional(value string) *string {
	if value == "" {
		return nil
	}
	return aws.String(value)
}
