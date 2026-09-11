package store

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type Config struct {
	Endpoint  string // http://127.0.0.1:9000
	AccessKey string
	SecretKey string
	Bucket    string
	Region    string
}

type Store struct {
	client    *s3.Client
	presigner *s3.PresignClient
	cfg       Config
}

func New(ctx context.Context, c Config) (*Store, error) {
	awsCfg, err := awscfg.LoadDefaultConfig(ctx,
		awscfg.WithRegion(orDefault(c.Region, "us-east-1")),
		awscfg.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, "")),
	)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(c.Endpoint)
		o.UsePathStyle = true // MinIO 本地部署使用 path-style
	})
	s := &Store{
		client:    client,
		presigner: s3.NewPresignClient(client, s3.WithPresignExpires(15*time.Minute)),
		cfg:       c,
	}
	if err := s.ensureBucket(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) ensureBucket(ctx context.Context) error {
	if _, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.cfg.Bucket)}); err == nil {
		s.ensureCORS(ctx)
		return nil
	}
	if _, err := s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(s.cfg.Bucket)}); err != nil {
		return fmt.Errorf("create bucket: %w", err)
	}
	// 1 天后中止未完成的分片上传，作为存储侧兜底（账本侧由 TTL + sweeper 立即释放）。
	// 该策略非关键路径：失败仅记录，不阻断启动。
	if _, err := s.client.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: aws.String(s.cfg.Bucket),
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{
			Rules: []types.LifecycleRule{{
				ID:     aws.String("abort-incomplete-mpu"),
				Status: types.ExpirationStatusEnabled,
				Filter: &types.LifecycleRuleFilter{Prefix: aws.String("")},
				AbortIncompleteMultipartUpload: &types.AbortIncompleteMultipartUpload{
					DaysAfterInitiation: aws.Int32(1),
				},
			}},
		},
	}); err != nil {
		log.Printf("store: set bucket lifecycle (best-effort): %v", err)
	}
	s.ensureCORS(ctx)
	return nil
}

// ensureCORS 允许浏览器用预签 URL 直传分片；凭据本身绝不下发。
func (s *Store) ensureCORS(ctx context.Context) {
	_, _ = s.client.PutBucketCors(ctx, &s3.PutBucketCorsInput{
		Bucket: aws.String(s.cfg.Bucket),
		CORSConfiguration: &types.CORSConfiguration{
			CORSRules: []types.CORSRule{{
				AllowedOrigins: []string{"*"},
				AllowedMethods: []string{"GET", "PUT", "HEAD", "DELETE"},
				AllowedHeaders: []string{"*"},
				ExposeHeaders:  []string{"ETag"},
				MaxAgeSeconds:  aws.Int32(3600),
			}},
		},
	})
}

// SafeKey 防止路径穿越与越权：key 只能由安全字符组成，且归一化后不含 ../ 与前导斜杠。
func SafeKey(teamID int64, key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 512 {
		return "", errors.New("非法对象名")
	}
	if strings.Contains(key, "..") || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") {
		return "", errors.New("非法对象名")
	}
	for _, r := range key {
		if r < 0x20 {
			return "", errors.New("非法对象名")
		}
	}
	// 团队隔离前缀：即便不同团队用了同名对象也互不影响
	return fmt.Sprintf("teams/%d/%s", teamID, key), nil
}

func (s *Store) key(teamID int64, objectKey string) (string, error) {
	if strings.HasPrefix(objectKey, "teams/") {
		// 内部已经是全路径
		if _, err := url.Parse(objectKey); err != nil {
			return "", err
		}
		return objectKey, nil
	}
	return SafeKey(teamID, objectKey)
}

func (s *Store) BeginMultipart(ctx context.Context, teamID int64, objectKey, contentType string) (fullKey, uploadID string, err error) {
	fullKey, err = s.key(teamID, objectKey)
	if err != nil {
		return "", "", err
	}
	in := &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(s.cfg.Bucket),
		Key:         aws.String(fullKey),
		ContentType: aws.String(orDefault(contentType, "application/octet-stream")),
	}
	out, err := s.client.CreateMultipartUpload(ctx, in)
	if err != nil {
		return "", "", fmt.Errorf("create multipart: %w", err)
	}
	return fullKey, aws.ToString(out.UploadId), nil
}

// PresignPutPart 为某个分片生成预签 PUT URL（S3 UploadPart 操作）。
func (s *Store) PresignPutPart(ctx context.Context, fullKey, uploadID string, partNumber int) (string, error) {
	out, err := s.presigner.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(s.cfg.Bucket),
		Key:        aws.String(fullKey),
		UploadId:   aws.String(uploadID),
		PartNumber: aws.Int32(int32(partNumber)),
	}, s3.WithPresignExpires(15*time.Minute))
	if err != nil {
		return "", err
	}
	return out.URL, nil
}

// PresignGetObject 为下载生成短期 GET URL。
func (s *Store) PresignGetObject(ctx context.Context, fullKey string) (string, error) {
	out, err := s.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(fullKey),
	})
	if err != nil {
		return "", err
	}
	return out.URL, nil
}

type CompletedPart struct {
	PartNumber int
	ETag       string
}

// CompleteMultipart 完成分片上传，返回最终对象字节数（以服务端实测为准）。
func (s *Store) CompleteMultipart(ctx context.Context, fullKey, uploadID string, parts []CompletedPart) (int64, error) {
	completed := make([]types.CompletedPart, 0, len(parts))
	for _, p := range parts {
		etag := strings.Trim(p.ETag, `"`)
		completed = append(completed, types.CompletedPart{
			ETag:       aws.String(etag),
			PartNumber: aws.Int32(int32(p.PartNumber)),
		})
	}
	if _, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.cfg.Bucket),
		Key:      aws.String(fullKey),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: completed,
		},
	}); err != nil {
		return 0, fmt.Errorf("complete multipart: %w", err)
	}
	h, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(fullKey),
	})
	if err != nil {
		return 0, fmt.Errorf("head object: %w", err)
	}
	return aws.ToInt64(h.ContentLength), nil
}

func (s *Store) AbortMultipart(ctx context.Context, fullKey, uploadID string) error {
	if uploadID == "" {
		return nil
	}
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.cfg.Bucket),
		Key:      aws.String(fullKey),
		UploadId: aws.String(uploadID),
	})
	return err
}

func (s *Store) DeleteObject(ctx context.Context, fullKey string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(fullKey),
	})
	return err
}

// HeadObjectSize 直接探测对象大小（校验存储侧实际占用）。
func (s *Store) HeadObjectSize(ctx context.Context, fullKey string) (int64, error) {
	h, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.cfg.Bucket), Key: aws.String(fullKey),
	})
	if err != nil {
		return 0, err
	}
	return aws.ToInt64(h.ContentLength), nil
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
