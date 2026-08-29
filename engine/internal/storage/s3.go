package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Config configures an S3-compatible backend (MinIO or AWS S3).
type S3Config struct {
	Endpoint     string // e.g. http://localhost:9000 ; empty = AWS
	Region       string
	Bucket       string
	AccessKey    string
	SecretKey    string
	UsePathStyle bool // required for MinIO
}

// S3Storage implements Storage against S3 / MinIO.
type S3Storage struct {
	client *s3.Client
	bucket string
}

// NewS3Storage builds a Storage client. Endpoint empty ⇒ real AWS.
func NewS3Storage(cfg S3Config) *S3Storage {
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	awsCfg := aws.Config{
		Region: cfg.Region,
		Credentials: credentials.NewStaticCredentialsProvider(
			cfg.AccessKey, cfg.SecretKey, "",
		),
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.UsePathStyle
	})
	return &S3Storage{client: client, bucket: cfg.Bucket}
}

func (s *S3Storage) EnsureBucket(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	if err == nil {
		return nil
	}
	_, err = s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(s.bucket)})
	if err != nil {
		var already *types.BucketAlreadyOwnedByYou
		var exists *types.BucketAlreadyExists
		if errors.As(err, &already) || errors.As(err, &exists) {
			return nil
		}
		if _, headErr := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)}); headErr == nil {
			return nil
		}
		return fmt.Errorf("create bucket %q: %w", s.bucket, err)
	}
	return nil
}

func (s *S3Storage) Put(ctx context.Context, key string, body io.Reader, size int64, opts PutOptions) (string, error) {
	in := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   body,
	}
	if size >= 0 {
		in.ContentLength = aws.Int64(size)
	}
	if opts.ContentType != "" {
		in.ContentType = aws.String(opts.ContentType)
	}
	if opts.IfNoneMatchStar {
		in.IfNoneMatch = aws.String("*")
	}
	if opts.IfMatchETag != "" {
		in.IfMatch = aws.String(opts.IfMatchETag)
	}

	out, err := s.client.PutObject(ctx, in)
	if err != nil {
		if isPreconditionFailed(err) {
			return "", ErrPreconditionFailed
		}
		return "", fmt.Errorf("put %q: %w", key, err)
	}
	if out.ETag == nil {
		return "", nil
	}
	return *out.ETag, nil
}

func (s *S3Storage) Get(ctx context.Context, key string) (io.ReadCloser, ObjectMeta, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ObjectMeta{}, ErrNotFound
		}
		return nil, ObjectMeta{}, fmt.Errorf("get %q: %w", key, err)
	}
	meta := ObjectMeta{Key: key}
	if out.ContentLength != nil {
		meta.Size = *out.ContentLength
	}
	if out.ETag != nil {
		meta.ETag = *out.ETag
	}
	if out.ContentType != nil {
		meta.ContentType = *out.ContentType
	}
	return out.Body, meta, nil
}

func (s *S3Storage) Head(ctx context.Context, key string) (ObjectMeta, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return ObjectMeta{}, ErrNotFound
		}
		return ObjectMeta{}, fmt.Errorf("head %q: %w", key, err)
	}
	meta := ObjectMeta{Key: key}
	if out.ContentLength != nil {
		meta.Size = *out.ContentLength
	}
	if out.ETag != nil {
		meta.ETag = *out.ETag
	}
	if out.ContentType != nil {
		meta.ContentType = *out.ContentType
	}
	return meta, nil
}

func (s *S3Storage) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("delete %q: %w", key, err)
	}
	return nil
}

func (s *S3Storage) List(ctx context.Context, prefix string, limit int) ([]ObjectMeta, error) {
	in := &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	}
	if limit > 0 {
		in.MaxKeys = aws.Int32(int32(limit))
	}
	out, err := s.client.ListObjectsV2(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("list %q: %w", prefix, err)
	}
	res := make([]ObjectMeta, 0, len(out.Contents))
	for _, obj := range out.Contents {
		m := ObjectMeta{}
		if obj.Key != nil {
			m.Key = *obj.Key
		}
		if obj.Size != nil {
			m.Size = *obj.Size
		}
		if obj.ETag != nil {
			m.ETag = *obj.ETag
		}
		res = append(res, m)
	}
	return res, nil
}

func isNotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey", "NoSuchBucket":
			return true
		}
	}
	var notFound *types.NotFound
	var noKey *types.NoSuchKey
	return errors.As(err, &notFound) || errors.As(err, &noKey)
}

func isPreconditionFailed(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		if code == "PreconditionFailed" || code == "412" {
			return true
		}
	}
	var resp interface{ HTTPStatusCode() int }
	if errors.As(err, &resp) && resp.HTTPStatusCode() == http.StatusPreconditionFailed {
		return true
	}
	return false
}
