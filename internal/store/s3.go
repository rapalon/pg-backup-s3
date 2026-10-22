package store

// feature/s3/manager is marked deprecated in favour of feature/s3/transfermanager,
// which is still a 0.x module with a moving API (see DECISIONS.md). manager keeps
// shipping with the SDK and is covered by the Garage integration tests; revisit
// when transfermanager reaches v1.
//
//lint:file-ignore SA1019 manager stays until transfermanager is stable

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Config is what NewS3 needs. The command layer fills it from config.S3.
type S3Config struct {
	Endpoint    string // URL with scheme; the scheme selects TLS
	Region      string
	Bucket      string
	AccessKey   string
	SecretKey   string
	PathStyle   bool
	PartSize    int64 // multipart part size; 32 MiB when zero
	TLSInsecure bool  // skip certificate verification (test stands only)
	MaxAttempts int   // per-request attempts including the first; 5 when zero
}

const (
	defaultPartSize       = 32 << 20
	defaultMaxAttempts    = 5
	deleteBatchSize       = 1000 // S3 DeleteObjects limit
	responseHeaderTimeout = 2 * time.Minute
)

// S3Store implements ObjectStore on aws-sdk-go-v2.
//
// Credentials are static and explicit: config.LoadDefaultConfig is never
// called, so nothing is read from ~/.aws, instance metadata or SSO.
// Checksum calculation and validation are set to WhenRequired because the
// SDK's default CRC checksums are rejected by some S3-compatible servers.
type S3Store struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
	partSize int64
}

// NewS3 builds the client. It does not touch the network; call Ping for that.
func NewS3(cfg S3Config) (*S3Store, error) {
	switch {
	case cfg.Endpoint == "":
		return nil, errors.New("s3: endpoint is required")
	case cfg.Bucket == "":
		return nil, errors.New("s3: bucket is required")
	case cfg.AccessKey == "" || cfg.SecretKey == "":
		return nil, errors.New("s3: access key and secret key are required")
	}
	partSize := cfg.PartSize
	if partSize <= 0 {
		partSize = defaultPartSize
	}
	if partSize < manager.MinUploadPartSize {
		return nil, fmt.Errorf("s3: part size %d is below the S3 minimum %d", partSize, manager.MinUploadPartSize)
	}
	attempts := cfg.MaxAttempts
	if attempts <= 0 {
		attempts = defaultMaxAttempts
	}

	// A server that accepts the connection and then never answers would
	// otherwise stall a part upload until BACKUP_TIMEOUT. The header timeout
	// starts after the request body was sent, so large parts are not cut off.
	httpClient := awshttp.NewBuildableClient().WithTransportOptions(func(tr *http.Transport) {
		tr.ResponseHeaderTimeout = responseHeaderTimeout
		if cfg.TLSInsecure {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit opt-in for test stands
		}
	})

	client := s3.New(s3.Options{
		BaseEndpoint:               aws.String(cfg.Endpoint),
		Region:                     cfg.Region,
		Credentials:                credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		UsePathStyle:               cfg.PathStyle,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		Retryer: retry.NewStandard(func(o *retry.StandardOptions) {
			o.MaxAttempts = attempts
		}),
		HTTPClient: httpClient,
	})

	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = partSize
		u.Concurrency = 1 // one part in flight bounds memory to ~2 parts
		u.LeavePartsOnError = false
	})

	return &S3Store{client: client, uploader: uploader, bucket: cfg.Bucket, partSize: partSize}, nil
}

// Ping checks that the bucket exists and the credentials can see it.
func (s *S3Store) Ping(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	if err == nil {
		return nil
	}
	var noBucket *types.NoSuchBucket
	if errors.As(err, &noBucket) || isNotFound(err) {
		return fmt.Errorf("%w: %q", ErrBucketNotFound, s.bucket)
	}
	return fmt.Errorf("bucket %q: %w", s.bucket, err)
}

// Put implements ObjectStore. Uploads larger than one part go through
// multipart; a failed upload is aborted by the SDK before Put returns.
func (s *S3Store) Put(ctx context.Context, key string, r io.Reader, opts PutOptions) (PutResult, error) {
	cr := &countingReader{r: r}
	input := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   cr,
	}
	if opts.ContentType != "" {
		input.ContentType = aws.String(opts.ContentType)
	}
	if len(opts.Metadata) > 0 {
		input.Metadata = opts.Metadata
	}
	var overrides []func(*manager.Uploader)
	if opts.PartSize > 0 {
		overrides = append(overrides, func(u *manager.Uploader) { u.PartSize = opts.PartSize })
	}
	out, err := s.uploader.Upload(ctx, input, overrides...)
	if err != nil {
		s.abortAfterFailure(ctx, key, err)
		return PutResult{}, fmt.Errorf("put %s: %w", key, err)
	}
	return PutResult{Size: cr.n, ETag: strings.Trim(aws.ToString(out.ETag), `"`)}, nil
}

// abortAfterFailure makes sure a failed multipart upload is aborted even when
// the failure was a cancelled context: the SDK aborts with the same context,
// which is already dead, so the abort would silently fail and leave parts
// behind. Aborting twice is harmless (NoSuchUpload is ignored).
func (s *S3Store) abortAfterFailure(ctx context.Context, key string, uploadErr error) {
	var mu manager.MultiUploadFailure
	if !errors.As(uploadErr, &mu) || mu.UploadID() == "" {
		return // single-part PutObject or no upload was started
	}
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_, _ = s.client.AbortMultipartUpload(abortCtx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(mu.UploadID()),
	})
}

// Get implements ObjectStore.
func (s *S3Store) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, ObjectInfo{}, wrapErr("get", key, err)
	}
	return out.Body, ObjectInfo{
		Key:          key,
		Size:         aws.ToInt64(out.ContentLength),
		LastModified: aws.ToTime(out.LastModified),
		Metadata:     out.Metadata,
	}, nil
}

// Stat implements ObjectStore.
func (s *S3Store) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return ObjectInfo{}, wrapErr("stat", key, err)
	}
	return ObjectInfo{
		Key:          key,
		Size:         aws.ToInt64(out.ContentLength),
		LastModified: aws.ToTime(out.LastModified),
		Metadata:     out.Metadata,
	}, nil
}

// List implements ObjectStore.
func (s *S3Store) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var infos []ObjectInfo
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, o := range page.Contents {
			infos = append(infos, ObjectInfo{
				Key:          aws.ToString(o.Key),
				Size:         aws.ToInt64(o.Size),
				LastModified: aws.ToTime(o.LastModified),
			})
		}
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Key < infos[j].Key })
	return infos, nil
}

// Delete implements ObjectStore.
func (s *S3Store) Delete(ctx context.Context, keys []string) error {
	for start := 0; start < len(keys); start += deleteBatchSize {
		end := min(start+deleteBatchSize, len(keys))
		ids := make([]types.ObjectIdentifier, 0, end-start)
		for _, k := range keys[start:end] {
			ids = append(ids, types.ObjectIdentifier{Key: aws.String(k)})
		}
		out, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("delete: %w", err)
		}
		var msgs []string
		for _, e := range out.Errors {
			switch aws.ToString(e.Code) {
			case "NoSuchKey", "NotFound":
				continue // AWS is silent about missing keys; some servers are not
			}
			msgs = append(msgs, fmt.Sprintf("%s: %s %s", aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message)))
		}
		if len(msgs) > 0 {
			return fmt.Errorf("delete: %d object(s) failed: %s", len(msgs), strings.Join(msgs, "; "))
		}
	}
	return nil
}

// IncompleteUploads lists multipart uploads under prefix that were started
// but neither completed nor aborted. Tests use it to prove failed uploads are
// cleaned up; operators can use it to audit a bucket.
func (s *S3Store) IncompleteUploads(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	input := &s3.ListMultipartUploadsInput{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix)}
	for {
		out, err := s.client.ListMultipartUploads(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("list multipart uploads: %w", err)
		}
		for _, u := range out.Uploads {
			keys = append(keys, aws.ToString(u.Key))
		}
		if !aws.ToBool(out.IsTruncated) || out.NextKeyMarker == nil {
			break
		}
		input.KeyMarker = out.NextKeyMarker
		input.UploadIdMarker = out.NextUploadIdMarker
	}
	return keys, nil
}

func wrapErr(op, key string, err error) error {
	if isNotFound(err) {
		return fmt.Errorf("%s %s: %w", op, key, ErrNotFound)
	}
	return fmt.Errorf("%s %s: %w", op, key, err)
}

// isNotFound recognises the several ways S3-compatible servers say 404.
func isNotFound(err error) bool {
	var noSuchKey *types.NoSuchKey
	var notFound *types.NotFound
	if errors.As(err, &noSuchKey) || errors.As(err, &notFound) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	return false
}
