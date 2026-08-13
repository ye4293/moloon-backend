package storage

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type R2Storage struct {
	client     *s3.Client
	bucket     string
	publicBase string
}

var _ Storage = (*R2Storage)(nil)

// NewR2Storage 构造打 Cloudflare R2 的 S3 客户端。
//
// endpoint 传完整地址（而非 account id）是为了让测试能指向 httptest.Server。
func NewR2Storage(endpoint, accessKeyID, secretAccessKey, bucket, publicBaseURL string) *R2Storage {
	client := s3.New(s3.Options{
		// R2 要求 region 固定为 "auto"。
		Region:       "auto",
		BaseEndpoint: aws.String(endpoint),
		// R2 用 path-style（/<bucket>/<key>）。virtual-host style 需要按桶名解析
		// 子域，R2 的 S3 endpoint 不提供。
		UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(
			accessKeyID, secretAccessKey, ""),
		// 只在上游明确要求时才算 checksum。
		//
		// **这不是"checksum 没用"**：默认的 WhenSupported 会附上
		// x-amz-checksum-crc32，R2 收到后会自己重算并在不一致时拒绝上传——那是
		// TLS 给不了的、落盘那一刻的完整性保证。选 WhenRequired 是拿它换两件事：
		// 一是 S3 兼容实现对这些头的处理并不一致，二是目前还没有真 R2 凭证，
		// 改不了也测不了。
		//
		// 等拿到真凭证跑通人工验证后，应当重新评估是否切回 WhenSupported。
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
	})
	return &R2Storage{
		client: client,
		bucket: bucket,
		// 去掉末尾斜杠：运维多打一个斜杠是必然会发生的事，而后果是每个 URL 里
		// 出现 // ，有些 CDN 会对此 404。
		publicBase: strings.TrimSuffix(publicBaseURL, "/"),
	}
}

func (s *R2Storage) Put(ctx context.Context, key, contentType string, body []byte) (string, error) {
	// 归一化后再用于 PutObject，避免对象键与 URL 各说一套（理由见 NormalizeKey）。
	key = NormalizeKey(key)
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String(contentType),
		// 显式给长度，而不是依赖 SDK 从 body 反推——bytes.Reader 可 seek，SDK 自己
		// 也能算出来，写出来只是让请求不依赖那一层推断。
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		return "", fmt.Errorf("上传对象 %s: %w", key, err)
	}
	return PublicURL(s.publicBase, key), nil
}
