package s3resource

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"net/http"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/cheggaaa/pb"
)

//go:generate counterfeiter . S3Client

type S3Client interface {
	BucketFiles(bucketName string, prefixHint string) ([]string, error)
	BucketFileVersions(bucketName string, remotePath string) ([]string, error)

	UploadFile(bucketName string, remotePath string, localPath string, options UploadFileOptions) (string, error)
	DownloadFile(bucketName string, remotePath string, versionID string, localPath string) error

	DeleteFile(bucketName string, remotePath string) error
	DeleteVersionedFile(bucketName string, remotePath string, versionID string) error

	URL(bucketName string, remotePath string, private bool, versionID string) string
}

// 12 retries works out to ~5 mins of total backoff time, though AWS randomizes
// the backoff to some extent so it may be as low as 4 or as high as 8 minutes
const maxRetries = 12

type s3client struct {
	client  *s3.S3
	session *session.Session

	progressOutput io.Writer
}

type UploadFileOptions struct {
	Acl                  string
	ServerSideEncryption string
	KmsKeyId             string
	ContentType          string
}

func NewUploadFileOptions() UploadFileOptions {
	return UploadFileOptions{
		Acl: "private",
	}
}

func NewS3Client(
	progressOutput io.Writer,
	awsConfig *aws.Config,
	useV2Signing bool,
) S3Client {
	sess := session.New(awsConfig)
	client := s3.New(sess, awsConfig)

	if useV2Signing {
		setv2Handlers(client)
	}

	return &s3client{
		client:  client,
		session: sess,

		progressOutput: progressOutput,
	}
}

type AwsConfigBuilder struct {
	AccessKey           string
	SecretKey           string
	SessionToken        string
	RegionName          string
	Endpoint            string
	DisableSSL          bool
	AssumeRoleArn       string
	SkipSSLVerification bool
}

func (b *AwsConfigBuilder) Build() *aws.Config {
	var providers []credentials.Provider
	var creds *credentials.Credentials

	// Add static access/secret or session token cred provider
	if b.AccessKey != "" || b.SessionToken != "" {
		creds := &credentials.StaticProvider{
			Value: credentials.Value{
				AccessKeyID:     b.AccessKey,
				SecretAccessKey: b.SecretKey,
				SessionToken:    b.SessionToken,
				ProviderName:    "Statically Defined",
			},
		}
		providers = append(providers, creds)
	}

	sess := session.Must(session.NewSession())

	// Add EC2 IAMRole provider (for folks running in AWS)
	providers = append(providers, &ec2rolecreds.EC2RoleProvider{
		Client: ec2metadata.New(sess),
	})

	// Append anonymous credentials for use with public S3 buckets, if all else fails
	providers = append(providers, &credentials.StaticProvider{})

	creds = credentials.NewChainCredentials(providers)

	if len(b.RegionName) == 0 {
		b.RegionName = "us-east-1"
	}

	var httpClient *http.Client
	if b.SkipSSLVerification {
		httpClient = &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}}
	} else {
		httpClient = http.DefaultClient
	}

	awsConfig := &aws.Config{
		Region:           aws.String(b.RegionName),
		Credentials:      creds,
		S3ForcePathStyle: aws.Bool(true),
		MaxRetries:       aws.Int(maxRetries),
		DisableSSL:       aws.Bool(b.DisableSSL),
		HTTPClient:       httpClient,
	}

	if len(b.Endpoint) != 0 {
		endpoint := fmt.Sprintf("%s", b.Endpoint)
		awsConfig.Endpoint = &endpoint
	}

	if b.AssumeRoleArn != "" {
		sess := session.Must(session.NewSession(awsConfig))
		creds := stscreds.NewCredentials(sess, b.AssumeRoleArn)
		awsConfig.Credentials = creds
	}

	return awsConfig
}

// Deprecated: use AwsConfigBuilder instead
func NewAwsConfig(
	accessKey string,
	secretKey string,
	sessionToken string,
	regionName string,
	endpoint string,
	disableSSL bool,
	skipSSLVerification bool,
	awsRoleArn string,
) *aws.Config {
	var creds *credentials.Credentials

	b := AwsConfigBuilder{
		AccessKey:           accessKey,
		SecretKey:           secretKey,
		SessionToken:        sessionToken,
		RegionName:          regionName,
		Endpoint:            endpoint,
		DisableSSL:          disableSSL,
		SkipSSLVerification: skipSSLVerification,
	}

	return b.Build()
}

func (client *s3client) BucketFiles(bucketName string, prefixHint string) ([]string, error) {
	entries, err := client.getBucketContents(bucketName, prefixHint)

	if err != nil {
		return []string{}, err
	}

	paths := make([]string, 0, len(entries))

	for _, entry := range entries {
		paths = append(paths, *entry.Key)
	}
	return paths, nil
}

func (client *s3client) BucketFileVersions(bucketName string, remotePath string) ([]string, error) {
	isBucketVersioned, err := client.getBucketVersioning(bucketName)
	if err != nil {
		return []string{}, err
	}

	if !isBucketVersioned {
		return []string{}, errors.New("bucket is not versioned")
	}

	bucketFiles, err := client.getVersionedBucketContents(bucketName, remotePath)

	if err != nil {
		return []string{}, err
	}

	versions := make([]string, 0, len(bucketFiles))

	for _, objectVersion := range bucketFiles[remotePath] {
		versions = append(versions, *objectVersion.VersionId)
	}

	return versions, nil
}

func (client *s3client) UploadFile(bucketName string, remotePath string, localPath string, options UploadFileOptions) (string, error) {
	uploader := s3manager.NewUploaderWithClient(client.client)

	if client.isGCSHost() {
		// GCS returns `InvalidArgument` on multipart uploads
		uploader.MaxUploadParts = 1
	}

	stat, err := os.Stat(localPath)
	if err != nil {
		return "", err
	}

	localFile, err := os.Open(localPath)
	if err != nil {
		return "", err
	}

	defer localFile.Close()
	
	// Automatically adjust partsize for larger files.
	fSize := stat.Size()
	if fSize > int64(uploader.MaxUploadParts) * uploader.PartSize {
		partSize := fSize / int64(uploader.MaxUploadParts)
		if fSize % int64(uploader.MaxUploadParts) != 0 {
			partSize++
		}
		uploader.PartSize = partSize
	}

	progress := client.newProgressBar(fSize)

	progress.Start()
	defer progress.Finish()

	uploadInput := s3manager.UploadInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(remotePath),
		Body:   progressReader{localFile, progress},
		ACL:    aws.String(options.Acl),
	}
	if options.ServerSideEncryption != "" {
		uploadInput.ServerSideEncryption = aws.String(options.ServerSideEncryption)
	}
	if options.KmsKeyId != "" {
		uploadInput.SSEKMSKeyId = aws.String(options.KmsKeyId)
	}
	if options.ContentType != "" {
		uploadInput.ContentType = aws.String(options.ContentType)
	}

	uploadOutput, err := uploader.Upload(&uploadInput)
	if err != nil {
		return "", err
	}

	if uploadOutput.VersionID != nil {
		return *uploadOutput.VersionID, nil
	}

	return "", nil
}

func (client *s3client) DownloadFile(bucketName string, remotePath string, versionID string, localPath string) error {
	headObject := &s3.HeadObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(remotePath),
	}

	if versionID != "" {
		headObject.VersionId = aws.String(versionID)
	}

	object, err := client.client.HeadObject(headObject)
	if err != nil {
		return err
	}

	progress := client.newProgressBar(*object.ContentLength)

	downloader := s3manager.NewDownloaderWithClient(client.client)

	localFile, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer localFile.Close()

	getObject := &s3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(remotePath),
	}

	if versionID != "" {
		getObject.VersionId = aws.String(versionID)
	}

	progress.Start()
	defer progress.Finish()

	_, err = downloader.Download(progressWriterAt{localFile, progress}, getObject)
	if err != nil {
		return err
	}

	return nil
}

func (client *s3client) URL(bucketName string, remotePath string, private bool, versionID string) string {
	getObjectInput := &s3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(remotePath),
	}

	if versionID != "" {
		getObjectInput.VersionId = aws.String(versionID)
	}

	awsRequest, _ := client.client.GetObjectRequest(getObjectInput)

	var url string

	if private {
		url, _ = awsRequest.Presign(24 * time.Hour)
	} else {
		awsRequest.Build()
		url = awsRequest.HTTPRequest.URL.String()
	}

	return url
}

func (client *s3client) DeleteVersionedFile(bucketName string, remotePath string, versionID string) error {
	_, err := client.client.DeleteObject(&s3.DeleteObjectInput{
		Bucket:    aws.String(bucketName),
		Key:       aws.String(remotePath),
		VersionId: aws.String(versionID),
	})

	return err
}

func (client *s3client) DeleteFile(bucketName string, remotePath string) error {
	_, err := client.client.DeleteObject(&s3.DeleteObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(remotePath),
	})

	return err
}

func (client *s3client) getBucketContents(bucketName string, prefix string) (map[string]*s3.Object, error) {
	bucketContents := map[string]*s3.Object{}
	marker := ""

	for {
		listObjectsResponse, err := client.client.ListObjects(&s3.ListObjectsInput{
			Bucket: aws.String(bucketName),
			Prefix: aws.String(prefix),
			Marker: aws.String(marker),
		})

		if err != nil {
			return bucketContents, err
		}

		lastKey := ""

		for _, key := range listObjectsResponse.Contents {
			bucketContents[*key.Key] = key

			lastKey = *key.Key
		}

		if *listObjectsResponse.IsTruncated {
			prevMarker := marker
			if listObjectsResponse.NextMarker == nil {
				// From the s3 docs: If response does not include the
				// NextMarker and it is truncated, you can use the value of the
				// last Key in the response as the marker in the subsequent
				// request to get the next set of object keys.
				marker = lastKey
			} else {
				marker = *listObjectsResponse.NextMarker
			}
			if marker == prevMarker {
				return nil, errors.New("Unable to list all bucket objects; perhaps this is a CloudFront S3 bucket that needs its `Query String Forwarding and Caching` set to `Forward all, cache based on all`?")
			}
		} else {
			break
		}
	}

	return bucketContents, nil
}

func (client *s3client) getBucketVersioning(bucketName string) (bool, error) {
	params := &s3.GetBucketVersioningInput{
		Bucket: aws.String(bucketName),
	}

	resp, err := client.client.GetBucketVersioning(params)
	if err != nil {
		return false, err
	}

	if resp.Status == nil {
		return false, nil
	}

	return *resp.Status == "Enabled", nil
}

func (client *s3client) getVersionedBucketContents(bucketName string, prefix string) (map[string][]*s3.ObjectVersion, error) {
	versionedBucketContents := map[string][]*s3.ObjectVersion{}
	keyMarker := ""
	versionMarker := ""
	for {

		params := &s3.ListObjectVersionsInput{
			Bucket:    aws.String(bucketName),
			KeyMarker: aws.String(keyMarker),
			Prefix:    aws.String(prefix),
		}

		if versionMarker != "" {
			params.VersionIdMarker = aws.String(versionMarker)
		}

		listObjectVersionsResponse, err := client.client.ListObjectVersions(params)
		if err != nil {
			return versionedBucketContents, err
		}

		lastKey := ""
		lastVersionKey := ""

		for _, objectVersion := range listObjectVersionsResponse.Versions {
			versionedBucketContents[*objectVersion.Key] = append(versionedBucketContents[*objectVersion.Key], objectVersion)

			lastKey = *objectVersion.Key
			lastVersionKey = *objectVersion.VersionId
		}

		if *listObjectVersionsResponse.IsTruncated {
			keyMarker = *listObjectVersionsResponse.NextKeyMarker
			versionMarker = *listObjectVersionsResponse.NextVersionIdMarker
			if keyMarker == "" {
				// From the s3 docs: If response does not include the
				// NextMarker and it is truncated, you can use the value of the
				// last Key in the response as the marker in the subsequent
				// request to get the next set of object keys.
				keyMarker = lastKey
			}

			if versionMarker == "" {
				versionMarker = lastVersionKey
			}
		} else {
			break
		}

	}

	return versionedBucketContents, nil
}

func (client *s3client) newProgressBar(total int64) *pb.ProgressBar {
	progress := pb.New64(total)

	progress.Output = client.progressOutput
	progress.ShowSpeed = true
	progress.Units = pb.U_BYTES
	progress.NotPrint = true

	return progress.SetWidth(80)
}

func (client *s3client) isGCSHost() bool {
	return (client.session.Config.Endpoint != nil && strings.Contains(*client.session.Config.Endpoint, "storage.googleapis.com"))
}
