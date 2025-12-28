package storage

import (
	"bytes"
	"context"
	"fmt"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type MinioConfig struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	UseSSL    bool
}

type Minio struct {
	client *minio.Client
}

func NewMinio(cfg MinioConfig) (*Minio, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create minio client: %w", err)
	}
	return &Minio{client: client}, nil
}

func (m *Minio) GetObject(bucket, object string) (*minio.Object, error) {
	return m.client.GetObject(context.Background(), bucket, object, minio.GetObjectOptions{})
}

func (m *Minio) PutObject(bucket, object string, data []byte) error {
	_, err := m.client.PutObject(context.Background(), bucket, object, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
	return err
}

func (m *Minio) EnsureBucket(bucket string) error {
	exists, err := m.client.BucketExists(context.Background(), bucket)
	if err != nil {
		return fmt.Errorf("failed to check bucket: %w", err)
	}
	if !exists {
		if err := m.client.MakeBucket(context.Background(), bucket, minio.MakeBucketOptions{}); err != nil {
			return fmt.Errorf("failed to create bucket: %w", err)
		}
	}
	return nil
}
