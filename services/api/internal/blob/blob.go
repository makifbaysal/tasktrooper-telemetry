// Package blob reads session replay segments from the S3-compatible blob store the ingest writer fills
// (contracts §12, §23): TT_S3_ENDPOINT, TT_S3_ACCESS_KEY, TT_S3_SECRET_KEY, TT_S3_REGION, bucket TT_REPLAY_S3_BUCKET
// (default tt-replays). Read only; the api never creates buckets or writes objects.
package blob

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/tasktrooper/telemetry/services/pkg/config"
)

// ErrNotFound is returned for a missing object (expired by the bucket lifecycle, or never written).
var ErrNotFound = errors.New("blob: not found")

// Reader opens objects by key.
type Reader interface {
	Open(ctx context.Context, key string) (io.ReadCloser, error)
}

// Config configures the store.
type Config struct {
	Endpoint, Bucket, AccessKey, SecretKey, Region string
	HTTPClient                                     *http.Client
}

// FromEnv returns the replay store, or nil when TT_S3_ENDPOINT is not set (replay segments then answer 503).
func FromEnv() *S3 {
	ep := config.String("TT_S3_ENDPOINT", "")
	if ep == "" {
		return nil
	}
	return NewS3(Config{Endpoint: ep, Bucket: config.String("TT_REPLAY_S3_BUCKET", "tt-replays"),
		AccessKey: config.String("TT_S3_ACCESS_KEY", ""), SecretKey: config.String("TT_S3_SECRET_KEY", ""),
		Region: config.String("TT_S3_REGION", "us-east-1")})
}

// S3 implements Reader on an S3-compatible endpoint (path-style; SeaweedFS in dev).
type S3 struct {
	c      *s3.Client
	bucket string
}

// NewS3 returns the client (no network calls).
func NewS3(cfg Config) *S3 {
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	o := s3.Options{
		Region:                     cfg.Region,
		BaseEndpoint:               aws.String(cfg.Endpoint),
		UsePathStyle:               true,
		Credentials:                credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	if cfg.HTTPClient != nil {
		o.HTTPClient = cfg.HTTPClient
	}
	return &S3{c: s3.New(o), bucket: cfg.Bucket}
}

// Open streams an object; the caller closes it.
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
