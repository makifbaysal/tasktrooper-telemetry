// Package blob stores session replay segments in an S3-compatible bucket (contracts §23; same client setup as
// services/symbolicator: aws-sdk-go-v2, path-style, SeaweedFS in dev).
package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Config configures the S3-compatible store (TT_S3_* env, bucket TT_REPLAY_S3_BUCKET).
type Config struct {
	Endpoint, Bucket, AccessKey, SecretKey, Region string
}

// S3 writes objects to one bucket.
type S3 struct {
	c      *s3.Client
	bucket string
}

// NewS3 creates the client and the bucket when missing.
func NewS3(ctx context.Context, cfg Config) (*S3, error) {
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	c := s3.New(s3.Options{
		Region:       cfg.Region,
		BaseEndpoint: aws.String(cfg.Endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		// SeaweedFS / MinIO: only send/validate checksums when required
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	o := &S3{c: c, bucket: cfg.Bucket}
	if _, err := c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &o.bucket}); err == nil {
		return o, nil
	}
	_, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &o.bucket})
	var owned *types.BucketAlreadyOwnedByYou
	var exists *types.BucketAlreadyExists
	if err != nil && !errors.As(err, &owned) && !errors.As(err, &exists) {
		return nil, fmt.Errorf("create bucket %s: %w", o.bucket, err)
	}
	return o, nil
}

// Put writes body under key (overwrites: a redelivered segment rewrites the same object).
func (o *S3) Put(ctx context.Context, key string, body []byte, contentType, contentEncoding string) error {
	in := &s3.PutObjectInput{Bucket: &o.bucket, Key: &key, Body: bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))), ContentType: aws.String(contentType)}
	if contentEncoding != "" {
		in.ContentEncoding = aws.String(contentEncoding)
	}
	_, err := o.c.PutObject(ctx, in)
	return err
}

// Ping checks the bucket.
func (o *S3) Ping(ctx context.Context) error {
	_, err := o.c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &o.bucket})
	return err
}
