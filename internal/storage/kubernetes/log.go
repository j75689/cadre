package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/sync/errgroup"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/j75689/cadre/internal/storage"
)

// S3Config says where the log lives.
type S3Config struct {
	Endpoint  string // empty for AWS S3
	Region    string
	Bucket    string
	Prefix    string // prepended to every object key, e.g. "cadre/"
	AccessKey string // with SecretKey; when either is empty the SDK's default
	SecretKey string // credential chain is used, for workload identity
	PathStyle bool   // required by most self-hosted S3-compatible servers
}

// objectLog is the append-only log on object storage: one object per entry,
// claimed with If-None-Match.
type objectLog struct {
	s3     *s3.Client
	bucket string
	prefix string
}

func newObjectLog(ctx context.Context, cfg S3Config) (*objectLog, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("s3: bucket is required")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	// Static keys only when both are given; otherwise the SDK's default
	// chain applies, which covers workload identity (IRSA, Pod Identity,
	// instance profiles) and the standard AWS environment variables.
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("s3: load AWS config: %w", err)
	}
	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		awsCfg.Credentials = credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = nonEmpty(cfg.Endpoint)
		o.UsePathStyle = cfg.PathStyle
		// Checksums only where S3 requires them, so S3-compatible servers that
		// do not implement the newer checksum headers work too.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
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
// should be retried once it settles.
const conditionalRetries = 6

// conflictBackoff waits a jittered, growing interval before retrying a
// ConditionalRequestConflict, so the retries outlast the competing write.
func conflictBackoff(ctx context.Context, attempt int) error {
	base := 20 * time.Millisecond << attempt
	timer := time.NewTimer(base/2 + rand.N(base))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (l *objectLog) Append(ctx context.Context, stream, key string, data []byte) error {
	if err := errors.Join(storage.ValidateStream(stream), storage.ValidateKey(key)); err != nil {
		return err
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
			return l.taken(ctx, stream, key, objKey)
		case code == "ConditionalRequestConflict" && attempt < conditionalRetries:
			if err := conflictBackoff(ctx, attempt); err != nil {
				return err
			}
		default:
			return fmt.Errorf("log %s entry %s: %w", stream, key, err)
		}
	}
}

// taken explains a 412 on Append. Usually the key holds an entry. But a store
// backed by a filesystem also answers 412 when the key is the directory of a
// nested stream, and then no entry exists: reporting ErrAlreadyExists would
// make an idempotent caller drop the entry silently.
func (l *objectLog) taken(ctx context.Context, stream, key, objKey string) error {
	var err error
	for attempt := 0; ; attempt++ {
		_, err = l.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &l.bucket, Key: &objKey})
		code := errorCode(err)
		var missing *types.NotFound
		switch {
		case err == nil:
			return fmt.Errorf("log %s entry %s: %w", stream, key, storage.ErrAlreadyExists)
		case errors.As(err, &missing) || code == "NotFound" || code == "NoSuchKey":
			return fmt.Errorf("log %s entry %s: the key is the path of a nested stream %s/%s/, not an entry: %w",
				stream, key, stream, key, storage.ErrStreamCollision)
		}
		// A transient failure: an ordinary race for a key must not turn
		// into an error because the check could not be made at once.
		if attempt == conditionalRetries {
			return fmt.Errorf("log %s entry %s: check a refused append: %w", stream, key, err)
		}
		if err := conflictBackoff(ctx, attempt); err != nil {
			return err
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
	if err == nil {
		return fmt.Errorf("probe object store %s: %w", l.bucket, errNotEnforced)
	}
	if errorCode(err) == "PreconditionFailed" {
		return nil
	}
	return fmt.Errorf("probe object store: second write: %w", err)
}

// readParallelism bounds the concurrent GETs of one Read.
const readParallelism = 16

func (l *objectLog) Read(ctx context.Context, stream, after string, limit int) ([]storage.Entry, error) {
	var out []storage.Entry
	err := l.list(ctx, stream, after, limit, func(prefix string, objs []types.Object) error {
		entries, err := l.getAll(ctx, prefix, objs)
		out = append(out, entries...)
		return err
	})
	return out, err
}

// Keys lists keys and sizes from ListObjectsV2 alone, with no GET.
func (l *objectLog) Keys(ctx context.Context, stream, after string, limit int) ([]storage.KeyInfo, error) {
	var out []storage.KeyInfo
	err := l.list(ctx, stream, after, limit, func(prefix string, objs []types.Object) error {
		for _, obj := range objs {
			out = append(out, storage.KeyInfo{Key: strings.TrimPrefix(*obj.Key, prefix), Size: aws.ToInt64(obj.Size)})
		}
		return nil
	})
	return out, err
}

// list pages through a stream's objects in key order, handing each page to
// page, until limit objects (zero or less: all) have been listed.
func (l *objectLog) list(ctx context.Context, stream, after string, limit int, page func(prefix string, objs []types.Object) error) error {
	if err := storage.ValidateStream(stream); err != nil {
		return err
	}
	prefix := l.streamPrefix(stream)
	// The delimiter keeps nested streams out: their objects are rolled up
	// into CommonPrefixes, which are ignored.
	in := &s3.ListObjectsV2Input{Bucket: &l.bucket, Prefix: &prefix, Delimiter: aws.String("/")}
	if after != "" {
		in.StartAfter = aws.String(prefix + after)
	}
	listed := 0
	for {
		if limit > 0 {
			in.MaxKeys = aws.Int32(int32(min(limit-listed, 1000)))
		}
		res, err := l.s3.ListObjectsV2(ctx, in)
		if err != nil {
			return fmt.Errorf("list log %s: %w", stream, err)
		}
		if err := page(prefix, res.Contents); err != nil {
			return err
		}
		listed += len(res.Contents)
		if !aws.ToBool(res.IsTruncated) || (limit > 0 && listed >= limit) {
			return nil
		}
		in.ContinuationToken = res.NextContinuationToken
	}
}

// getAll fetches the listed objects concurrently, keeping their order.
func (l *objectLog) getAll(ctx context.Context, prefix string, objs []types.Object) ([]storage.Entry, error) {
	out := make([]storage.Entry, len(objs))
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(readParallelism)
	for i, obj := range objs {
		g.Go(func() error {
			data, err := l.get(ctx, *obj.Key)
			out[i] = storage.Entry{Key: strings.TrimPrefix(*obj.Key, prefix), Data: data}
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
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
