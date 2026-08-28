package infrastructure

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3ObjectStore 使用 AWS SDK v2 的标准 S3 API，兼容 AWS、阿里云 OSS S3、MinIO 等服务。
type S3ObjectStore struct {
	client         *s3.Client
	bucket, prefix string
	maxReadBytes   int64
}

// S3ObjectStoreOptions 描述生产对象存储连接；静态凭据为空时使用 SDK 默认凭据链。
type S3ObjectStoreOptions struct {
	Bucket, Prefix, Endpoint, Region           string
	AccessKeyID, SecretAccessKey, SessionToken string
	UsePathStyle                               bool
	MaxReadBytes                               int64
}

// Ready 验证生产对象存储 Bucket 可访问。它不写测试对象，仅用于启动编排的就绪探针。
func (store *S3ObjectStore) Ready(ctx context.Context) error {
	if store == nil || store.client == nil || strings.TrimSpace(store.bucket) == "" {
		return errors.New("S3 object store is not configured")
	}
	if _, err := store.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &store.bucket}); err != nil {
		return fmt.Errorf("head S3 bucket: %w", err)
	}
	return nil
}

// NewS3ObjectStore 构造 S3 适配器，并验证 endpoint 不含用户信息或查询参数。
func NewS3ObjectStore(ctx context.Context, options S3ObjectStoreOptions) (*S3ObjectStore, error) {
	if strings.TrimSpace(options.Bucket) == "" {
		return nil, errors.New("S3 bucket is required")
	}
	if options.Region == "" {
		options.Region = "us-east-1"
	}
	if options.MaxReadBytes <= 0 {
		options.MaxReadBytes = 100 << 20
	}
	if options.Endpoint != "" {
		parsed, err := url.Parse(options.Endpoint)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, errors.New("S3 endpoint must be an HTTP(S) URL without query or credentials")
		}
	}
	loadOptions := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(options.Region)}
	if options.AccessKeyID != "" || options.SecretAccessKey != "" {
		if options.AccessKeyID == "" || options.SecretAccessKey == "" {
			return nil, errors.New("S3 access key and secret must be provided together")
		}
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(options.AccessKeyID, options.SecretAccessKey, options.SessionToken)))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("load S3 configuration: %w", err)
	}
	client := s3.NewFromConfig(cfg, func(client *s3.Options) {
		client.UsePathStyle = options.UsePathStyle
		if options.Endpoint != "" {
			client.BaseEndpoint = &options.Endpoint
		}
	})
	return &S3ObjectStore{client: client, bucket: options.Bucket, prefix: strings.Trim(options.Prefix, "/"), maxReadBytes: options.MaxReadBytes}, nil
}

// WriteAtomically 上传受限内容并返回真实长度及摘要；PutObject 的完整请求原子替换对象。
func (store *S3ObjectStore) WriteAtomically(ctx context.Context, relativePath string, content io.Reader, maxBytes int64) (uint64, []byte, error) {
	if content == nil || maxBytes <= 0 {
		return 0, nil, errors.New("object content and positive max bytes are required")
	}
	data, err := io.ReadAll(io.LimitReader(content, maxBytes+1))
	if err != nil || int64(len(data)) > maxBytes {
		return 0, nil, errors.New("object content exceeds configured maximum size")
	}
	digest := sha256.Sum256(data)
	key, err := store.key(relativePath)
	if err != nil {
		return 0, nil, err
	}
	_, err = store.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &store.bucket, Key: &key, Body: bytes.NewReader(data), ContentLength: int64Ptr(int64(len(data))), ContentType: stringPtr("application/octet-stream")})
	if err != nil {
		return 0, nil, fmt.Errorf("put S3 object: %w", err)
	}
	return uint64(len(data)), digest[:], nil
}

// OpenVerified 先 HeadObject，再按 maxReadBytes 限制 GetObject 响应，避免内存失控。
func (store *S3ObjectStore) OpenVerified(relativePath string) (io.ReadSeekCloser, error) {
	key, err := store.key(relativePath)
	if err != nil {
		return nil, err
	}
	head, err := store.client.HeadObject(context.Background(), &s3.HeadObjectInput{Bucket: &store.bucket, Key: &key})
	if err != nil {
		return nil, fmt.Errorf("head S3 object: %w", err)
	}
	if head.ContentLength == nil || *head.ContentLength < 0 || *head.ContentLength > store.maxReadBytes {
		return nil, errors.New("S3 object exceeds configured read limit")
	}
	object, err := store.client.GetObject(context.Background(), &s3.GetObjectInput{Bucket: &store.bucket, Key: &key})
	if err != nil {
		return nil, fmt.Errorf("get S3 object: %w", err)
	}
	defer object.Body.Close()
	data, err := io.ReadAll(io.LimitReader(object.Body, store.maxReadBytes+1))
	if err != nil || int64(len(data)) > store.maxReadBytes {
		return nil, errors.New("S3 object exceeds configured read limit")
	}
	return &seekReadCloser{Reader: bytes.NewReader(data)}, nil
}

// Remove 删除对象；S3 DeleteObject 对不存在对象天然幂等。
func (store *S3ObjectStore) Remove(relativePath string) error {
	key, err := store.key(relativePath)
	if err != nil {
		return err
	}
	_, err = store.client.DeleteObject(context.Background(), &s3.DeleteObjectInput{Bucket: &store.bucket, Key: &key})
	if err != nil {
		return fmt.Errorf("delete S3 object: %w", err)
	}
	return nil
}

// CleanupTemporary 由 S3 生命周期规则负责，避免网关执行无界对象列表。
func (store *S3ObjectStore) CleanupTemporary(time.Time) (int, error) { return 0, nil }
func (store *S3ObjectStore) key(value string) (string, error) {
	clean, err := objectKey(value)
	if err != nil {
		return "", err
	}
	return path.Join(store.prefix, clean), nil
}
func int64Ptr(value int64) *int64    { return &value }
func stringPtr(value string) *string { return &value }

type seekReadCloser struct{ *bytes.Reader }

func (seekReadCloser) Close() error { return nil }

func objectKey(value string) (string, error) {
	value = strings.Trim(strings.ReplaceAll(value, "\\", "/"), "/")
	// 即使 path.Clean 最终仍落在前缀内，也拒绝任何显式父目录片段，避免不同业务对象
	// 通过等价路径覆盖同一个对象键并破坏审计语义。
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return "", errors.New("unsafe object storage key")
		}
	}
	clean := path.Clean(value)
	if value == "" || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsRune(value, 0) {
		return "", errors.New("unsafe object storage key")
	}
	return clean, nil
}
