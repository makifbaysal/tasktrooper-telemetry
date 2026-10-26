package store

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Config configures the S3-compatible blob store (contracts.md §12).
type S3Config struct {
	Endpoint, Bucket, AccessKey, SecretKey, Region string
}

// S3 implements Objects on an S3-compatible endpoint (path-style; SeaweedFS in dev).
type S3 struct {
	c      *s3.Client
	bucket string
}

// NewS3 creates the client and the bucket when missing.
func NewS3(ctx context.Context, cfg S3Config) (*S3, error) {
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

func (o *S3) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, contentType string) error {
	_, err := o.c.PutObject(ctx, &s3.PutObjectInput{Bucket: &o.bucket, Key: &key, Body: body,
		ContentLength: aws.Int64(size), ContentType: aws.String(contentType)})
	return err
}

func (o *S3) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := o.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &o.bucket, Key: &key})
	if err != nil {
		var nsk *types.NoSuchKey
		var api smithy.APIError
		if errors.As(err, &nsk) || errors.As(err, &api) && (api.ErrorCode() == "NotFound" || api.ErrorCode() == "NoSuchKey") {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return out.Body, nil
}

func (o *S3) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	p := s3.NewListObjectsV2Paginator(o.c, &s3.ListObjectsV2Input{Bucket: &o.bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Contents {
			keys = append(keys, aws.ToString(obj.Key))
		}
	}
	return keys, nil
}

func (o *S3) Delete(ctx context.Context, key string) error {
	_, err := o.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &o.bucket, Key: aws.String(key)})
	return err
}

func (o *S3) Ping(ctx context.Context) error {
	_, err := o.c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &o.bucket})
	return err
}
