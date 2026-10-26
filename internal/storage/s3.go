package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// S3Config uses the AWS SDK's normal credential chain, including environment
// variables, shared configuration and workload roles. Credentials never belong
// in published deployment metadata.
type S3Config struct {
	Bucket    string `yaml:"bucket"`
	Prefix    string `yaml:"prefix"`
	Region    string `yaml:"region"`
	Endpoint  string `yaml:"endpoint"`
	PathStyle bool   `yaml:"path_style"`
}

type S3 struct {
	client *s3.Client
	bucket string
	prefix string
}

var _ Store = (*S3)(nil)

func NewS3(ctx context.Context, c S3Config) (*S3, error) {
	if strings.TrimSpace(c.Bucket) == "" {
		return nil, errors.New("S3 bucket is required")
	}
	if strings.HasPrefix(c.Prefix, "/") {
		return nil, fmt.Errorf("S3 prefix: %w", ErrInvalidKey)
	}
	prefix := strings.TrimRight(c.Prefix, "/")
	if err := validPrefix(prefix); err != nil {
		return nil, fmt.Errorf("S3 prefix: %w", err)
	}
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("S3 endpoint must be an http(s) URL without credentials, query or fragment")
		}
	}
	opts := []func(*config.LoadOptions) error{}
	if c.Region != "" {
		opts = append(opts, config.WithRegion(c.Region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	if cfg.Region == "" {
		return nil, errors.New("S3 region is required (region or AWS_REGION)")
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
		}
		o.UsePathStyle = c.PathStyle
		// Optional SDK checksums require seekable streams or aws-chunked
		// support. Known-length uploads use ordinary S3 PutObject instead.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	if prefix != "" {
		prefix += "/"
	}
	return &S3{client: client, bucket: c.Bucket, prefix: prefix}, nil
}

func classify(op, key string, err error) error {
	if err == nil {
		return nil
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return fmt.Errorf("%s %q: %w", op, key, ErrNotFound)
		}
	}
	return fmt.Errorf("%s %q: %w", op, key, err)
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, Info, error) {
	return s.get(ctx, key, "")
}

func (s *S3) GetRange(ctx context.Context, key string, off, length int64) (io.ReadCloser, Info, error) {
	if off < 0 || length <= 0 || off > math.MaxInt64-(length-1) {
		return nil, Info{}, errors.New("invalid object range")
	}
	return s.get(ctx, key, fmt.Sprintf("bytes=%d-%d", off, off+length-1))
}

func (s *S3) get(ctx context.Context, key, byteRange string) (io.ReadCloser, Info, error) {
	if err := validKey(key); err != nil {
		return nil, Info{}, err
	}
	in := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.prefix + key)}
	if byteRange != "" {
		in.Range = aws.String(byteRange)
	}
	out, err := s.client.GetObject(ctx, in)
	if err != nil {
		return nil, Info{}, classify("get", key, err)
	}
	info := Info{Key: key, Size: aws.ToInt64(out.ContentLength), ContentType: aws.ToString(out.ContentType), ETag: aws.ToString(out.ETag), Modified: aws.ToTime(out.LastModified)}
	return out.Body, info, nil
}

func (s *S3) Stat(ctx context.Context, key string) (Info, error) {
	if err := validKey(key); err != nil {
		return Info{}, err
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.prefix + key)})
	if err != nil {
		return Info{}, classify("stat", key, err)
	}
	return Info{Key: key, Size: aws.ToInt64(out.ContentLength), ContentType: aws.ToString(out.ContentType), ETag: aws.ToString(out.ETag), Modified: aws.ToTime(out.LastModified)}, nil
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) (Info, error) {
	if err := validKey(key); err != nil {
		return Info{}, err
	}
	if size < 0 || r == nil {
		return Info{}, errors.New("object body and nonnegative size are required")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	body := r
	if _, seekable := r.(io.Seeker); !seekable {
		// Smithy's HTTP adapter otherwise forces a bare *io.PipeReader to
		// chunked encoding even when the caller supplied a known length.
		body = struct{ io.Reader }{r}
	}
	out, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.prefix + key), Body: body, ContentLength: aws.Int64(size), ContentType: aws.String(contentType)}, func(o *s3.Options) {
		// The publisher may provide a file, archive stream or pipe. Avoid
		// pre-reading the entire body merely to compute a signing hash.
		o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
			if err := stack.Build.Add(middleware.BuildMiddlewareFunc("PageKnownContentLength", func(ctx context.Context, in middleware.BuildInput, next middleware.BuildHandler) (middleware.BuildOutput, middleware.Metadata, error) {
				request, ok := in.Request.(*smithyhttp.Request)
				if !ok {
					return middleware.BuildOutput{}, middleware.Metadata{}, errors.New("unexpected S3 request type")
				}
				request.ContentLength = size
				return next.HandleBuild(ctx, in)
			}), middleware.Before); err != nil {
				return err
			}
			return v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware(stack)
		})
	})
	if err != nil {
		return Info{}, classify("put", key, err)
	}
	return Info{Key: key, Size: size, ContentType: contentType, ETag: aws.ToString(out.ETag), Modified: time.Now().UTC()}, nil
}

func (s *S3) List(ctx context.Context, prefix, cursor string, limit int) (Page, error) {
	if err := validPrefix(prefix); err != nil {
		return Page{}, err
	}
	if limit <= 0 || limit > 1000 {
		return Page{}, errors.New("list limit must be between 1 and 1000")
	}
	in := &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(s.prefix + prefix), MaxKeys: aws.Int32(int32(limit))}
	if cursor != "" {
		in.ContinuationToken = aws.String(cursor)
	}
	out, err := s.client.ListObjectsV2(ctx, in)
	if err != nil {
		return Page{}, classify("list", prefix, err)
	}
	page := Page{NextCursor: aws.ToString(out.NextContinuationToken)}
	for _, item := range out.Contents {
		physical := aws.ToString(item.Key)
		if !strings.HasPrefix(physical, s.prefix) {
			return Page{}, errors.New("S3 list returned an object outside the configured prefix")
		}
		key := strings.TrimPrefix(physical, s.prefix)
		if err := validKey(key); err != nil {
			return Page{}, fmt.Errorf("listed object %q: %w", key, err)
		}
		page.Objects = append(page.Objects, Info{Key: key, Size: aws.ToInt64(item.Size), ETag: aws.ToString(item.ETag), Modified: aws.ToTime(item.LastModified)})
	}
	if aws.ToBool(out.IsTruncated) && page.NextCursor == "" {
		return Page{}, errors.New("S3 list was truncated without a continuation token")
	}
	return page, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if err := validKey(key); err != nil {
		return err
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.prefix + key)})
	return classify("delete", key, err)
}
