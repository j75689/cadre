package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/oklog/ulid/v2"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/j75689/cadre/internal/storage"
)

// S3Config says where the log lives.
type S3Config struct {
	Endpoint  string // empty for AWS S3
	Region    string
	Bucket    string
	Prefix    string // prepended to every object key, e.g. "cadre/"
	AccessKey string
	SecretKey string
	PathStyle bool // required by most self-hosted S3-compatible servers
}

// objectLog is the append-only log on object storage: one object per entry,
// claimed with If-None-Match.
type objectLog struct {
	s3     *s3.Client
	bucket string
	prefix string
}

func newObjectLog(cfg S3Config) (*objectLog, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("s3: bucket is required")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	client := s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: nonEmpty(cfg.Endpoint),
		UsePathStyle: cfg.PathStyle,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		// Checksums only where S3 requires them, so S3-compatible servers that
		// do not implement the newer checksum headers work too.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &objectLog{s3: client, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (l *objectLog) streamPrefix(stream string) string { return l.prefix + stream + "/" }

// conditionalRetries bounds retries of S3's 409 ConditionalRequestConflict,
// which means a concurrent conditional write was in flight and the request
// should be retried.
const conditionalRetries = 5

func (l *objectLog) Append(ctx context.Context, stream, key string, data []byte) error {
	if stream == "" || key == "" || strings.Contains(key, "/") {
		return fmt.Errorf("log entry %q in stream %q: key must be non-empty and contain no slash", key, stream)
	}
	objKey := l.streamPrefix(stream) + key
	for attempt := 0; ; attempt++ {
		_, err := l.s3.PutObject(ctx, &s3.PutObjectInput{
			Bucket:      &l.bucket,
			Key:         &objKey,
			Body:        bytes.NewReader(data),
			IfNoneMatch: aws.String("*"),
		})
		switch code := errorCode(err); {
		case err == nil:
			return nil
		case code == "PreconditionFailed":
			return fmt.Errorf("log %s entry %s: %w", stream, key, storage.ErrAlreadyExists)
		case code == "ConditionalRequestConflict" && attempt < conditionalRetries:
			continue
		default:
			return fmt.Errorf("log %s entry %s: %w", stream, key, err)
		}
	}
}

// errNotEnforced means the object store accepted a conditional write it should
// have refused.
var errNotEnforced = errors.New("object store ignores If-None-Match on PutObject")

// probe writes a scratch key twice with If-None-Match: * and requires the
// second write to be refused. A store that ignores the precondition would
// silently overwrite log entries, so the driver must not start on it. The key
// is unique per call and deleted afterwards.
func (l *objectLog) probe(ctx context.Context) error {
	key := l.prefix + ".probe/" + ulid.Make().String()
	put := func() error {
		_, err := l.s3.PutObject(ctx, &s3.PutObjectInput{
			Bucket: &l.bucket, Key: &key, Body: strings.NewReader("probe"), IfNoneMatch: aws.String("*"),
		})
		return err
	}
	if err := put(); err != nil {
		return fmt.Errorf("probe object store: %w", err)
	}
	defer func() {
		// Best effort: a leftover probe key is harmless.
		_, _ = l.s3.DeleteObject(context.WithoutCancel(ctx), &s3.DeleteObjectInput{Bucket: &l.bucket, Key: &key})
	}()
	err := put()
	switch errorCode(err) {
	case "PreconditionFailed":
		return nil
	case "":
		if err == nil {
			return fmt.Errorf("probe object store %s: %w", l.bucket, errNotEnforced)
		}
	}
	return fmt.Errorf("probe object store: second write: %w", err)
}

func (l *objectLog) Read(ctx context.Context, stream, after string, limit int) ([]storage.Entry, error) {
	prefix := l.streamPrefix(stream)
	in := &s3.ListObjectsV2Input{Bucket: &l.bucket, Prefix: &prefix}
	if after != "" {
		in.StartAfter = aws.String(prefix + after)
	}
	var out []storage.Entry
	for {
		if limit > 0 {
			in.MaxKeys = aws.Int32(int32(min(limit-len(out), 1000)))
		}
		page, err := l.s3.ListObjectsV2(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("list log %s: %w", stream, err)
		}
		for _, obj := range page.Contents {
			data, err := l.get(ctx, *obj.Key)
			if err != nil {
				return nil, err
			}
			out = append(out, storage.Entry{Key: strings.TrimPrefix(*obj.Key, prefix), Data: data})
		}
		if !aws.ToBool(page.IsTruncated) || (limit > 0 && len(out) >= limit) {
			return out, nil
		}
		in.ContinuationToken = page.NextContinuationToken
	}
}

func (l *objectLog) get(ctx context.Context, key string) ([]byte, error) {
	obj, err := l.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: &l.bucket, Key: &key})
	if err != nil {
		return nil, fmt.Errorf("get log entry %s: %w", key, err)
	}
	defer func() { _ = obj.Body.Close() }()
	return io.ReadAll(obj.Body)
}

func errorCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}
