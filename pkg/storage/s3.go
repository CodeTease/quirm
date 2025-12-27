package storage

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	appConfig "github.com/CodeTease/quirm/pkg/config"
	"github.com/CodeTease/quirm/pkg/metrics"
)

type S3Client struct {
	client       *s3.Client
	bucket       string
	backupBucket string
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

	return &S3Client{
		client:       client,
		bucket:       cfg.S3Bucket,
		backupBucket: cfg.S3BackupBucket,
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
		if s.backupBucket != "" {
			// Check if error is NoSuchKey or equivalent
			// AWS SDK v2 errors are checked differently.
			// Simplified: We assume almost any error on GET except context cancel might be worth trying backup?
			// But spec says "If bucket fails or file not found".
			// So let's try backup.
			
			// We could log here
			// slog.Info("Primary bucket fetch failed, trying backup", "error", err, "bucket", s.backupBucket)
			
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
			// If backup fails, return ORIGINAL error usually, or backup error?
			// Typically original error is more relevant if both fail, unless backup error is "found but ..."
			// Let's return the original error to keep semantics, or maybe wrapping?
			// But if backup also 404s, returning original 404 is fine.
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
