package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	appConfig "github.com/CodeTease/quirm/pkg/config"
	"github.com/CodeTease/quirm/pkg/metrics"
)

type S3Client struct {
	client        *s3.Client
	presignClient *s3.PresignClient
	bucket        string
	backupBucket  string
}

// Ensure S3Client implements StorageProvider
var _ StorageProvider = (*S3Client)(nil)

func NewS3Client(cfg appConfig.Config) (*S3Client, error) {
	clientLogMode := aws.LogRequest
	if !cfg.Debug {
		clientLogMode = aws.ClientLogMode(0)
	}

	awsCfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion(cfg.S3Region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.S3AccessKey, cfg.S3SecretKey, "")),
		config.WithClientLogMode(clientLogMode),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.S3Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.S3Endpoint)
		}
		o.UsePathStyle = cfg.S3ForcePathStyle
		if cfg.S3UseCustomDomain {
			o.EndpointResolver = s3.EndpointResolverFunc(func(region string, options s3.EndpointResolverOptions) (aws.Endpoint, error) {
				return aws.Endpoint{
					URL:               cfg.S3Endpoint,
					HostnameImmutable: true,
					SigningRegion:     cfg.S3Region,
					Source:            aws.EndpointSourceCustom,
				}, nil
			})
			o.APIOptions = []func(*middleware.Stack) error{
				func(stack *middleware.Stack) error {
					return stack.Finalize.Add(middleware.FinalizeMiddlewareFunc("StripBucketFromPath",
						func(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (
							middleware.FinalizeOutput, middleware.Metadata, error,
						) {
							req, ok := in.Request.(*smithyhttp.Request)
							if !ok {
								return next.HandleFinalize(ctx, in)
							}
							prefix := "/" + cfg.S3Bucket
							if strings.HasPrefix(req.URL.Path, prefix) {
								req.URL.Path = strings.TrimPrefix(req.URL.Path, prefix)
							}
							return next.HandleFinalize(ctx, in)
						}),
						middleware.Before,
					)
				},
			}
		}
	})

	presignClient := s3.NewPresignClient(client)

	return &S3Client{
		client:        client,
		presignClient: presignClient,
		bucket:        cfg.S3Bucket,
		backupBucket:  cfg.S3BackupBucket,
	}, nil
}

func (s *S3Client) GetObject(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	start := time.Now()
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		// Failover Logic
		if s.backupBucket != "" && shouldFailover(err) {
			respBackup, errBackup := s.client.GetObject(ctx, &s3.GetObjectInput{
				Bucket: aws.String(s.backupBucket),
				Key:    aws.String(key),
			})
			if errBackup == nil {
				metrics.S3FetchDuration.Observe(time.Since(start).Seconds())
				var contentLength int64
				if respBackup.ContentLength != nil {
					contentLength = *respBackup.ContentLength
				}
				return respBackup.Body, contentLength, nil
			}
		}

		return nil, 0, err
	}

	// Only record metric if configured (implicit check: if metrics initialized)
	// We can check appConfig, but here we don't have it easily accessible unless stored.
	// However, prometheus metrics are global and safe to call even if not scraped,
	// unless we want to avoid the overhead.
	// Given the instructions, we should just record it.
	// But wait, the plan said "Optional".
	// The metrics variables are global. If we record them, they just update in memory.
	// If /metrics is not exposed, no one sees them. That's fine.
	// The overhead is minimal.
	metrics.S3FetchDuration.Observe(time.Since(start).Seconds())

	var contentLength int64
	if resp.ContentLength != nil {
		contentLength = *resp.ContentLength
	}
	return resp.Body, contentLength, nil
}

func (s *S3Client) GetPresignedURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	request, err := s.presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, func(o *s3.PresignOptions) {
		o.Expires = expiry
	})
	if err != nil {
		return "", fmt.Errorf("failed to presign request: %w", err)
	}
	return request.URL, nil
}

func shouldFailover(err error) bool {
	// 1. Check specific API error codes (e.g. "NoSuchKey")
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		if code == "NoSuchKey" || code == "NotFound" {
			return true
		}
	}

	// 2. Check HTTP Status Codes via ResponseError
	var respErr *smithyhttp.ResponseError
	if errors.As(err, &respErr) {
		status := respErr.Response.StatusCode
		if status == http.StatusNotFound || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
			return true
		}
		if status >= 500 {
			return true
		}
		// Client error (4xx) that isn't 404/408/429 -> Do NOT failover
		if status >= 400 && status < 500 {
			return false
		}
	}

	// 3. Generic/Network errors -> Failover as safety net
	return true
}
