package media

import (
	"bytes"
	"context"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type R2Config struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
}

type R2 struct {
	client *s3.Client
	bucket string
}

func NewR2(cfg R2Config) *R2 {
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(cfg.Endpoint),
		Region:       "auto",
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		UsePathStyle: true,
	})
	return &R2{client: client, bucket: cfg.Bucket}
}

func (r *R2) Put(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := r.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(r.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(int64(len(data))),
	})
	return err
}

func (r *R2) Delete(ctx context.Context, key string) error {
	_, err := r.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
	})
	return err
}

func publicURL(base, key string) string {
	base = strings.TrimRight(base, "/")
	if base == "" {
		base = "https://cdn.yildizskylab.com"
	}
	return base + "/" + strings.TrimLeft(key, "/")
}
