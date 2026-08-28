package infrastructure

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// TestS3ObjectStoreIntegration 使用显式提供的 S3 兼容服务验证真实网络写入、读取和删除。
// 默认跳过，CI 或发布前通过 FILE_GATEWAY_S3_INTEGRATION_ENDPOINT 启用，测试凭据不得提交。
func TestS3ObjectStoreIntegration(t *testing.T) {
	endpoint := os.Getenv("FILE_GATEWAY_S3_INTEGRATION_ENDPOINT")
	if endpoint == "" {
		t.Skip("FILE_GATEWAY_S3_INTEGRATION_ENDPOINT is not configured")
	}
	bucket := envOr("FILE_GATEWAY_S3_INTEGRATION_BUCKET", "file-gateway-integration")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewS3ObjectStore(ctx, S3ObjectStoreOptions{
		Endpoint: endpoint, Bucket: bucket, Prefix: "integration", Region: envOr("FILE_GATEWAY_S3_INTEGRATION_REGION", "us-east-1"),
		AccessKeyID: os.Getenv("FILE_GATEWAY_S3_INTEGRATION_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("FILE_GATEWAY_S3_INTEGRATION_SECRET_ACCESS_KEY"),
		UsePathStyle: true, MaxReadBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("NewS3ObjectStore() error = %v", err)
	}
	if _, err = store.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil && !bucketAlreadyExists(err) {
		t.Fatalf("CreateBucket() error = %v", err)
	}
	if err = store.Ready(ctx); err != nil {
		t.Fatalf("Ready() error = %v", err)
	}

	content := []byte("file gateway production adapter integration")
	size, digest, err := store.WriteAtomically(ctx, "tenant/application/file.txt", bytes.NewReader(content), 1024)
	if err != nil {
		t.Fatalf("WriteAtomically() error = %v", err)
	}
	wantDigest := sha256.Sum256(content)
	if size != uint64(len(content)) || !bytes.Equal(digest, wantDigest[:]) {
		t.Fatalf("WriteAtomically() size/digest mismatch")
	}
	reader, err := store.OpenVerified("tenant/application/file.txt")
	if err != nil {
		t.Fatalf("OpenVerified() error = %v", err)
	}
	actual, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(actual, content) {
		t.Fatalf("OpenVerified() content mismatch: read=%v close=%v", readErr, closeErr)
	}
	if err = store.Remove("tenant/application/file.txt"); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err = store.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("integration/tenant/application/file.txt")}); err == nil {
		t.Fatal("HeadObject() succeeded after deletion")
	}
}

func bucketAlreadyExists(err error) bool {
	var apiError smithy.APIError
	if !errors.As(err, &apiError) {
		return false
	}
	return apiError.ErrorCode() == "BucketAlreadyOwnedByYou" || apiError.ErrorCode() == "BucketAlreadyExists"
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
