package media

import (
	"github.com/Willh92/Volo-go/internal/s3client"
	"github.com/Willh92/Volo-go/internal/security"
)

// S3Config aliases s3client.Config for media storage configuration.
type S3Config = s3client.Config

// S3Client aliases s3client.Client for pure-Go SigV4 S3/R2/MinIO storage.
type S3Client = s3client.Client

// NewS3Client constructs a pure-Go SigV4 S3 client.
func NewS3Client(cfg S3Config, safeDialer *security.OutboundSafeDialer) (*S3Client, error) {
	return s3client.NewClient(cfg, safeDialer)
}
