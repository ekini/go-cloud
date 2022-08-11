// Copyright 2018 The Go Cloud Development Kit Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package azureblob provides a blob implementation that uses Azure Storage’s
// BlockBlob. Use OpenBucket to construct a *blob.Bucket.
//
// NOTE: SignedURLs for PUT created with this package are not fully portable;
// they will not work unless the PUT request includes a "x-ms-blob-type" header
// set to "BlockBlob".
// See https://stackoverflow.com/questions/37824136/put-on-sas-blob-url-without-specifying-x-ms-blob-type-header.
//
// # URLs
//
// For blob.OpenBucket, azureblob registers for the scheme "azblob".
//
// The default URL opener will use environment variables to generate
// credentials and a service URL; see
// https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/sdk/storage/azblob
// for a more complete descriptions of each approach.
//   - AZURE_STORAGE_ACCOUNT: The service account name. Required if not set as "storage_account" in the URL query string parameter.
//   - AZURE_STORAGE_KEY: To use a shared key credential. The service account
//     name and key are passed to NewSharedKeyCredential and then the
//     resulting credential is passed to NewServiceClientWithSharedKey.
//   - AZURE_STORAGE_CONNECTION_STRING: To use a connection string, passed to
//     NewServiceClientFromConnectionString.
//   - AZURE_STORAGE_SAS_TOKEN: To use a SAS token. The SAS token is added
//     as a URL parameter to the service URL, and passed to
//     NewServiceClientWithNoCredential.
//   - If none of the above are provided, azureblob defaults to
//     azidentity.NewDefaultAzureCredential:
//     https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/sdk/azidentity#NewDefaultAzureCredential.
//     See the documentation there for the environment variables it supports,
//     including AZURE_CLIENT_ID, AZURE_TENANT_ID, etc.
//
// In addition, the environment variables AZURE_STORAGE_DOMAIN,
// AZURE_STORAGE_PROTOCOL, AZURE_STORAGE_IS_CDN, and AZURE_STORAGE_IS_LOCAL_EMULATOR
// can be used to configure how the default URLOpener generates the Azure
// Service URL via ServiceURLOptions. These can all be configured via URL
// parameters as well. See ServiceURLOptions and NewDefaultServiceURL
// for more details.
//
// To customize the URL opener, or for more details on the URL format,
// see URLOpener.
//
// See https://gocloud.dev/concepts/urls/ for background information.
//
// # Escaping
//
// Go CDK supports all UTF-8 strings; to make this work with services lacking
// full UTF-8 support, strings must be escaped (during writes) and unescaped
// (during reads). The following escapes are performed for azureblob:
//   - Blob keys: ASCII characters 0-31, 92 ("\"), and 127 are escaped to
//     "__0x<hex>__". Additionally, the "/" in "../" and a trailing "/" in a
//     key (e.g., "foo/") are escaped in the same way.
//   - Metadata keys: Per https://docs.microsoft.com/en-us/azure/storage/blobs/storage-properties-metadata,
//     Azure only allows C# identifiers as metadata keys. Therefore, characters
//     other than "[a-z][A-z][0-9]_" are escaped using "__0x<hex>__". In addition,
//     characters "[0-9]" are escaped when they start the string.
//     URL encoding would not work since "%" is not valid.
//   - Metadata values: Escaped using URL encoding.
//
// # As
//
// azureblob exposes the following types for As:
//   - Bucket: *azblob.ContainerClient
//   - Error: *azcore.ReponseError, *azblob.InternalError, *azblob.StorageError
//   - ListObject: azblob.BlobItemInternal for objects, azblob.BlobPrefix for "directories"
//   - ListOptions.BeforeList: *azblob.ContainerListBlobsHierarchyOption
//   - Reader: azblob.BlobDownloadResponse
//   - Reader.BeforeRead: *azblob.BlockDownloadOptions
//   - Attributes: azblob.BlobGetPropertiesResponse
//   - CopyOptions.BeforeCopy: *azblob.BlobStartCopyOptions
//   - WriterOptions.BeforeWrite: *azblob.UploadStreamOptions
//   - SignedURLOptions.BeforeSign: *azblob.BlobSASPermissions
package azureblob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/google/wire"
	"gocloud.dev/blob"
	"gocloud.dev/blob/driver"
	"gocloud.dev/gcerrors"

	"gocloud.dev/internal/escape"
	"gocloud.dev/internal/gcerr"
	"gocloud.dev/internal/useragent"
)

const (
	defaultMaxDownloadRetryRequests = 3               // download retry policy (Azure default is zero)
	defaultPageSize                 = 1000            // default page size for ListPaged (Azure default is 5000)
	defaultUploadBuffers            = 5               // configure the number of rotating buffers that are used when uploading (for degree of parallelism)
	defaultUploadBlockSize          = 8 * 1024 * 1024 // configure the upload buffer size
)

func init() {
	blob.DefaultURLMux().RegisterBucket(Scheme, new(lazyOpener))
}

// Set holds Wire providers for this package.
var Set = wire.NewSet(
	NewDefaultServiceURLOptions,
	NewServiceURL,
	NewDefaultServiceClient,
)

// Options sets options for constructing a *blob.Bucket backed by Azure Blob.
type Options struct{}

// ServiceURL represents an Azure service URL.
type ServiceURL string

// ServiceURLOptions sets options for constructing a service URL for Azure Blob.
type ServiceURLOptions struct {
	// AccountName is the account name the credentials are for.
	AccountName string

	// SASToken will be appended to the service URL.
	// See https://docs.microsoft.com/en-us/azure/storage/common/storage-dotnet-shared-access-signature-part-1#shared-access-signature-parameters.
	SASToken string

	// StorageDomain can be provided to specify an Azure Cloud Environment
	// domain to target for the blob storage account (i.e. public, government, china).
	// Defaults to "blob.core.windows.net". Possible values will look similar
	// to this but are different for each cloud (i.e. "blob.core.govcloudapi.net" for USGovernment).
	// Check the Azure developer guide for the cloud environment where your bucket resides.
	// See the docstring for NewServiceURL to see examples of how this is used
	// along with the other Options fields.
	StorageDomain string

	// Protocol can be provided to specify protocol to access Azure Blob Storage.
	// Protocols that can be specified are "http" for local emulator and "https" for general.
	// Defaults to "https".
	// See the docstring for NewServiceURL to see examples of how this is used
	// along with the other Options fields.
	Protocol string

	// IsCDN can be set to true when using a CDN URL pointing to a blob storage account:
	// https://docs.microsoft.com/en-us/azure/cdn/cdn-create-a-storage-account-with-cdn
	// See the docstring for NewServiceURL to see examples of how this is used
	// along with the other Options fields.
	IsCDN bool

	// IsLocalEmulator should be set to true when targeting Local Storage Emulator (Azurite).
	// See the docstring for NewServiceURL to see examples of how this is used
	// along with the other Options fields.
	IsLocalEmulator bool
}

// NewDefaultServiceURLOptions generates a ServiceURLOptions based on environment variables.
func NewDefaultServiceURLOptions() *ServiceURLOptions {
	isCDN, _ := strconv.ParseBool(os.Getenv("AZURE_STORAGE_IS_CDN"))
	isLocalEmulator, _ := strconv.ParseBool(os.Getenv("AZURE_STORAGE_IS_LOCAL_EMULATOR"))
	return &ServiceURLOptions{
		AccountName:     os.Getenv("AZURE_STORAGE_ACCOUNT"),
		SASToken:        os.Getenv("AZURE_STORAGE_SAS_TOKEN"),
		StorageDomain:   os.Getenv("AZURE_STORAGE_DOMAIN"),
		Protocol:        os.Getenv("AZURE_STORAGE_PROTOCOL"),
		IsCDN:           isCDN,
		IsLocalEmulator: isLocalEmulator,
	}
}

// withOverrides returns o with overrides from urlValues.
// See URLOpener for supported overrides.
func (o *ServiceURLOptions) withOverrides(urlValues url.Values) (*ServiceURLOptions, error) {
	retval := *o
	for param, values := range urlValues {
		if len(values) > 1 {
			return nil, fmt.Errorf("multiple values of %v not allowed", param)
		}
		value := values[0]
		switch param {
		case "domain":
			retval.StorageDomain = value
		case "protocol":
			retval.Protocol = value
		case "cdn":
			isCDN, err := strconv.ParseBool(value)
			if err != nil {
				return nil, err
			}
			retval.IsCDN = isCDN
		case "localemu":
			isLocalEmulator, err := strconv.ParseBool(value)
			if err != nil {
				return nil, err
			}
			retval.IsLocalEmulator = isLocalEmulator
		case "storage_account":
			retval.AccountName = value
		default:
			return nil, fmt.Errorf("unknown query parameter %q", param)
		}
	}
	return &retval, nil
}

// NewServiceURL generates a URL for addressing an Azure Blob service
// account. It uses several parameters, each of which can be specified
// via ServiceURLOptions.
//
// The generated URL is "<protocol>://<account name>.<domain>"
// with the following caveats:
//   - If opts.SASToken is provided, it is appended to the URL as a query
//     parameter.
//   - If opts.IsCDN is true, the <account name> part is dropped.
//   - If opts.IsLocalEmulator is true, or the domain starts with "localhost"
//     or "127.0.0.1", the account name and domain are flipped, e.g.:
//     http://127.0.0.1:10000/myaccount
func NewServiceURL(opts *ServiceURLOptions) (ServiceURL, error) {
	if opts == nil {
		opts = &ServiceURLOptions{}
	}
	accountName := opts.AccountName
	if accountName == "" {
		return "", errors.New("azureblob: Options.AccountName is required")
	}
	domain := opts.StorageDomain
	if domain == "" {
		domain = "blob.core.windows.net"
	}
	protocol := opts.Protocol
	if protocol == "" {
		protocol = "https"
	} else if protocol != "http" && protocol != "https" {
		return "", fmt.Errorf("invalid protocol %q", protocol)
	}
	var svcURL string
	if strings.HasPrefix(domain, "127.0.0.1") || strings.HasPrefix(domain, "localhost") || opts.IsLocalEmulator {
		svcURL = fmt.Sprintf("%s://%s/%s", protocol, domain, accountName)
	} else if opts.IsCDN {
		svcURL = fmt.Sprintf("%s://%s", protocol, domain)
	} else {
		svcURL = fmt.Sprintf("%s://%s.%s", protocol, accountName, domain)
	}
	if opts.SASToken != "" {
		svcURL += "?" + opts.SASToken
	}
	log.Printf("azureblob: constructed service URL: %s\n", svcURL)
	return ServiceURL(svcURL), nil
}

// lazyOpener obtains credentials and creates a client on the first call to OpenBucketURL.
type lazyOpener struct {
	init   sync.Once
	opener *URLOpener
}

func (o *lazyOpener) OpenBucketURL(ctx context.Context, u *url.URL) (*blob.Bucket, error) {
	o.init.Do(func() {
		credInfo := newCredInfoFromEnv()
		opts := NewDefaultServiceURLOptions()
		o.opener = &URLOpener{
			MakeClient:        credInfo.NewServiceClient,
			ServiceURLOptions: *opts,
		}
	})
	return o.opener.OpenBucketURL(ctx, u)
}

type credTypeEnumT int

const (
	credTypeSharedKey credTypeEnumT = iota
	credTypeSASViaNone
	credTypeConnectionString
	credTypeIdentityFromEnv
)

type credInfoT struct {
	CredType credTypeEnumT

	// For credTypeSharedKey.
	AccountName string
	AccountKey  string

	// For credTypeSASViaNone.
	//SASToken string

	// For credTypeConnectionString
	ConnectionString string
}

func newCredInfoFromEnv() *credInfoT {
	accountName := os.Getenv("AZURE_STORAGE_ACCOUNT")
	accountKey := os.Getenv("AZURE_STORAGE_KEY")
	sasToken := os.Getenv("AZURE_STORAGE_SAS_TOKEN")
	connectionString := os.Getenv("AZURE_STORAGE_CONNECTION_STRING")
	credInfo := &credInfoT{
		AccountName: accountName,
	}
	if accountName != "" && accountKey != "" {
		credInfo.CredType = credTypeSharedKey
		credInfo.AccountKey = accountKey
	} else if sasToken != "" {
		credInfo.CredType = credTypeSASViaNone
		//credInfo.SASToken = sasToken
	} else if connectionString != "" {
		credInfo.CredType = credTypeConnectionString
		credInfo.ConnectionString = connectionString
	} else {
		credInfo.CredType = credTypeIdentityFromEnv
	}
	return credInfo
}

func (i *credInfoT) NewServiceClient(svcURL ServiceURL) (*azblob.ServiceClient, error) {
	// Set the ApplicationID.
	azClientOpts := &azblob.ClientOptions{
		Telemetry: policy.TelemetryOptions{
			ApplicationID: useragent.AzureUserAgentPrefix("blob"),
		},
	}

	switch i.CredType {
	case credTypeSharedKey:
		log.Println("azureblob.URLOpener: using shared key credentials")
		sharedKeyCred, err := azblob.NewSharedKeyCredential(i.AccountName, i.AccountKey)
		if err != nil {
			return nil, fmt.Errorf("failed azblob.NewSharedKeyCredential: %v", err)
		}
		return azblob.NewServiceClientWithSharedKey(string(svcURL), sharedKeyCred, azClientOpts)
	case credTypeSASViaNone:
		log.Println("azureblob.URLOpener: using SAS token and no other credentials")
		return azblob.NewServiceClientWithNoCredential(string(svcURL), azClientOpts)
	case credTypeConnectionString:
		log.Println("azureblob.URLOpener: using connection string")
		return azblob.NewServiceClientFromConnectionString(i.ConnectionString, azClientOpts)
	case credTypeIdentityFromEnv:
		log.Println("azureblob.URLOpener: using NewEnvironmentCredentials")
		cred, err := azidentity.NewEnvironmentCredential(nil)
		if err != nil {
			return nil, fmt.Errorf("failed azidentity.NewEnvironmentCredential: %v", err)
		}
		return azblob.NewServiceClient(string(svcURL), cred, azClientOpts)
	default:
		return nil, errors.New("internal error, unknown cred type")
	}
}

// Scheme is the URL scheme gcsblob registers its URLOpener under on
// blob.DefaultMux.
const Scheme = "azblob"

// URLOpener opens Azure URLs like "azblob://mybucket".
//
// The URL host is used as the bucket name.
//
// The following query options are supported:
//   - domain: Overrides Options.StorageDomain.
//   - protocol: Overrides Options.Protocol.
//   - cdn: Overrides Options.IsCDN.
//   - localemu: Overrides Options.IsLocalEmulator.
type URLOpener struct {
	// MakeClient must be set to a non-nil value.
	MakeClient func(svcURL ServiceURL) (*azblob.ServiceClient, error)

	// ServiceURLOptions specifies default options for generating the service URL.
	// Some options can be overridden in the URL as described above.
	ServiceURLOptions ServiceURLOptions

	// Options specifies the options to pass to OpenBucket.
	Options Options
}

// OpenBucketURL opens a blob.Bucket based on u.
func (o *URLOpener) OpenBucketURL(ctx context.Context, u *url.URL) (*blob.Bucket, error) {
	opts, err := o.ServiceURLOptions.withOverrides(u.Query())
	if err != nil {
		return nil, err
	}
	svcURL, err := NewServiceURL(opts)
	if err != nil {
		return nil, err
	}
	svcClient, err := o.MakeClient(svcURL)
	if err != nil {
		return nil, err
	}
	return OpenBucket(ctx, svcClient, u.Host, &o.Options)
}

// bucket represents a Azure Storage Account Container, which handles read,
// write and delete operations on objects within it.
// See https://docs.microsoft.com/en-us/azure/storage/blobs/storage-blobs-introduction.
type bucket struct {
	client *azblob.ContainerClient
	opts   *Options
}

// NewDefaultServiceClient returns an Azure Blob service client
// with credentials from the environment as described in the package
// docstring.
func NewDefaultServiceClient(svcURL ServiceURL) (*azblob.ServiceClient, error) {
	return newCredInfoFromEnv().NewServiceClient(svcURL)
}

// OpenBucket returns a *blob.Bucket backed by Azure Storage Account. See the package
// documentation for an example and
// https://godoc.org/github.com/Azure/azure-storage-blob-go/azblob
// for more details.
func OpenBucket(ctx context.Context, svcClient *azblob.ServiceClient, containerName string, opts *Options) (*blob.Bucket, error) {
	b, err := openBucket(ctx, svcClient, containerName, opts)
	if err != nil {
		return nil, err
	}
	return blob.NewBucket(b), nil
}

func openBucket(ctx context.Context, svcClient *azblob.ServiceClient, containerName string, opts *Options) (*bucket, error) {
	if svcClient == nil {
		return nil, errors.New("azureblob.OpenBucket: client is required")
	}
	if containerName == "" {
		return nil, errors.New("azureblob.OpenBucket: containerName is required")
	}
	containerClient, err := svcClient.NewContainerClient(containerName)
	if err != nil {
		return nil, err
	}
	if opts == nil {
		opts = &Options{}
	}
	return &bucket{
		client: containerClient,
		opts:   opts,
	}, nil
}

// Close implements driver.Close.
func (b *bucket) Close() error {
	return nil
}

// Copy implements driver.Copy.
func (b *bucket) Copy(ctx context.Context, dstKey, srcKey string, opts *driver.CopyOptions) error {
	dstKey = escapeKey(dstKey, false)
	dstBlobClient, err := b.client.NewBlobClient(dstKey)
	if err != nil {
		return err
	}
	srcKey = escapeKey(srcKey, false)
	srcBlobClient, err := b.client.NewBlobClient(srcKey)
	if err != nil {
		return err
	}
	copyOptions := &azblob.BlobStartCopyOptions{}
	if opts.BeforeCopy != nil {
		asFunc := func(i interface{}) bool {
			switch v := i.(type) {
			case **azblob.BlobStartCopyOptions:
				*v = copyOptions
				return true
			}
			return false
		}
		if err := opts.BeforeCopy(asFunc); err != nil {
			return err
		}
	}
	resp, err := dstBlobClient.StartCopyFromURL(ctx, srcBlobClient.URL(), copyOptions)
	if err != nil {
		return err
	}
	nErrors := 0
	copyStatus := *resp.CopyStatus
	for copyStatus == azblob.CopyStatusTypePending {
		// Poll until the copy is complete.
		time.Sleep(500 * time.Millisecond)
		propertiesResp, err := dstBlobClient.GetProperties(ctx, nil)
		if err != nil {
			// A GetProperties failure may be transient, so allow a couple
			// of them before giving up.
			nErrors++
			if ctx.Err() != nil || nErrors == 3 {
				return err
			}
		}
		copyStatus = *propertiesResp.CopyStatus
	}
	if copyStatus != azblob.CopyStatusTypeSuccess {
		return fmt.Errorf("Copy failed with status: %s", copyStatus)
	}
	return nil
}

// Delete implements driver.Delete.
func (b *bucket) Delete(ctx context.Context, key string) error {
	key = escapeKey(key, false)
	blobClient, err := b.client.NewBlobClient(key)
	if err != nil {
		return err
	}
	_, err = blobClient.Delete(ctx, nil)
	return err
}

// reader reads an azblob. It implements io.ReadCloser.
type reader struct {
	body  io.ReadCloser
	attrs driver.ReaderAttributes
	raw   *azblob.BlobDownloadResponse
}

func (r *reader) Read(p []byte) (int, error) {
	return r.body.Read(p)
}
func (r *reader) Close() error {
	return r.body.Close()
}
func (r *reader) Attributes() *driver.ReaderAttributes {
	return &r.attrs
}
func (r *reader) As(i interface{}) bool {
	p, ok := i.(*azblob.BlobDownloadResponse)
	if !ok {
		return false
	}
	*p = *r.raw
	return true
}

// NewRangeReader implements driver.NewRangeReader.
func (b *bucket) NewRangeReader(ctx context.Context, key string, offset, length int64, opts *driver.ReaderOptions) (driver.Reader, error) {
	key = escapeKey(key, false)
	blobClient, err := b.client.NewBlobClient(key)

	downloadOpts := azblob.BlobDownloadOptions{Offset: &offset}
	if length >= 0 {
		downloadOpts.Count = &length
	}
	if opts.BeforeRead != nil {
		asFunc := func(i interface{}) bool {
			if p, ok := i.(**azblob.BlobDownloadOptions); ok {
				*p = &downloadOpts
				return true
			}
			return false
		}
		if err := opts.BeforeRead(asFunc); err != nil {
			return nil, err
		}
	}
	blobDownloadResponse, err := blobClient.Download(ctx, &downloadOpts)
	if err != nil {
		return nil, err
	}
	attrs := driver.ReaderAttributes{
		ContentType: to.String(blobDownloadResponse.ContentType),
		Size:        getSize(*blobDownloadResponse.ContentLength, to.String(blobDownloadResponse.ContentRange)),
		ModTime:     *blobDownloadResponse.LastModified,
	}
	var body io.ReadCloser
	if length == 0 {
		body = http.NoBody
	} else {
		body = blobDownloadResponse.Body(&azblob.RetryReaderOptions{MaxRetryRequests: defaultMaxDownloadRetryRequests})
	}
	return &reader{
		body:  body,
		attrs: attrs,
		raw:   &blobDownloadResponse,
	}, nil
}

func getSize(contentLength int64, contentRange string) int64 {
	// Default size to ContentLength, but that's incorrect for partial-length reads,
	// where ContentLength refers to the size of the returned Body, not the entire
	// size of the blob. ContentRange has the full size.
	size := contentLength
	if contentRange != "" {
		// Sample: bytes 10-14/27 (where 27 is the full size).
		parts := strings.Split(contentRange, "/")
		if len(parts) == 2 {
			if i, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
				size = i
			}
		}
	}
	return size
}

// As implements driver.As.
func (b *bucket) As(i interface{}) bool {
	p, ok := i.(**azblob.ContainerClient)
	if !ok {
		return false
	}
	*p = b.client
	return true
}

// As implements driver.ErrorAs.
func (b *bucket) ErrorAs(err error, i interface{}) bool {
	switch v := err.(type) {
	case *azcore.ResponseError:
		if p, ok := i.(**azcore.ResponseError); ok {
			*p = v
			return true
		}
	case *azblob.StorageError:
		if p, ok := i.(**azblob.StorageError); ok {
			*p = v
			return true
		}
	case *azblob.InternalError:
		if p, ok := i.(**azblob.InternalError); ok {
			*p = v
			return true
		}
	}
	return false
}

func (b *bucket) ErrorCode(err error) gcerrors.ErrorCode {
	var errorCode azblob.StorageErrorCode
	var statusCode int
	var sErr *azblob.StorageError
	var rErr *azcore.ResponseError
	if errors.As(err, &sErr) {
		errorCode = sErr.ErrorCode
		statusCode = sErr.StatusCode()
	} else if errors.As(err, &rErr) {
		errorCode = azblob.StorageErrorCode(rErr.ErrorCode)
		statusCode = rErr.StatusCode
	} else if strings.Contains(err.Error(), "no such host") {
		// This happens with an invalid storage account name; the host
		// is something like invalidstorageaccount.blob.core.windows.net.
		return gcerrors.NotFound
	} else {
		return gcerrors.Unknown
	}
	if errorCode == azblob.StorageErrorCodeBlobNotFound || statusCode == 404 {
		return gcerrors.NotFound
	}
	if errorCode == azblob.StorageErrorCodeAuthenticationFailed {
		return gcerrors.PermissionDenied
	}
	return gcerrors.Unknown
}

// Attributes implements driver.Attributes.
func (b *bucket) Attributes(ctx context.Context, key string) (*driver.Attributes, error) {
	key = escapeKey(key, false)
	blobClient, err := b.client.NewBlobClient(key)
	if err != nil {
		return nil, err
	}
	blobPropertiesResponse, err := blobClient.GetProperties(ctx, nil)
	if err != nil {
		return nil, err
	}

	md := make(map[string]string, len(blobPropertiesResponse.Metadata))
	for k, v := range blobPropertiesResponse.Metadata {
		// See the package comments for more details on escaping of metadata
		// keys & values.
		md[escape.HexUnescape(k)] = escape.URLUnescape(v)
	}
	return &driver.Attributes{
		CacheControl:       to.String(blobPropertiesResponse.CacheControl),
		ContentDisposition: to.String(blobPropertiesResponse.ContentDisposition),
		ContentEncoding:    to.String(blobPropertiesResponse.ContentEncoding),
		ContentLanguage:    to.String(blobPropertiesResponse.ContentLanguage),
		ContentType:        to.String(blobPropertiesResponse.ContentType),
		Size:               to.Int64(blobPropertiesResponse.ContentLength),
		CreateTime:         *blobPropertiesResponse.CreationTime,
		ModTime:            *blobPropertiesResponse.LastModified,
		MD5:                blobPropertiesResponse.ContentMD5,
		ETag:               to.String(blobPropertiesResponse.ETag),
		Metadata:           md,
		AsFunc: func(i interface{}) bool {
			p, ok := i.(*azblob.BlobGetPropertiesResponse)
			if !ok {
				return false
			}
			*p = blobPropertiesResponse
			return true
		},
	}, nil
}

// ListPaged implements driver.ListPaged.
func (b *bucket) ListPaged(ctx context.Context, opts *driver.ListOptions) (*driver.ListPage, error) {
	pageSize := opts.PageSize
	if pageSize == 0 {
		pageSize = defaultPageSize
	}

	var marker *string
	if len(opts.PageToken) > 0 {
		pt := string(opts.PageToken)
		marker = &pt
	}

	pageSize32 := int32(pageSize)
	prefix := escapeKey(opts.Prefix, true)
	azOpts := azblob.ContainerListBlobsHierarchyOptions{
		MaxResults: &pageSize32,
		Prefix:     &prefix,
		Marker:     marker,
	}
	if opts.BeforeList != nil {
		asFunc := func(i interface{}) bool {
			p, ok := i.(**azblob.ContainerListBlobsHierarchyOptions)
			if !ok {
				return false
			}
			*p = &azOpts
			return true
		}
		if err := opts.BeforeList(asFunc); err != nil {
			return nil, err
		}
	}
	azPager := b.client.ListBlobsHierarchy(escapeKey(opts.Delimiter, true), &azOpts)
	azPager.NextPage(ctx)
	if err := azPager.Err(); err != nil {
		return nil, err
	}
	resp := azPager.PageResponse()
	page := &driver.ListPage{}
	page.Objects = []*driver.ListObject{}
	segment := resp.ListBlobsHierarchySegmentResponse.Segment
	for _, blobPrefix := range segment.BlobPrefixes {
		blobPrefix := blobPrefix // capture loop variable for use in AsFunc
		page.Objects = append(page.Objects, &driver.ListObject{
			Key:   unescapeKey(to.String(blobPrefix.Name)),
			Size:  0,
			IsDir: true,
			AsFunc: func(i interface{}) bool {
				p, ok := i.(*azblob.BlobPrefix)
				if !ok {
					return false
				}
				*p = *blobPrefix
				return true
			}})
	}
	for _, blobInfo := range segment.BlobItems {
		blobInfo := blobInfo // capture loop variable for use in AsFunc
		page.Objects = append(page.Objects, &driver.ListObject{
			Key:     unescapeKey(to.String(blobInfo.Name)),
			ModTime: *blobInfo.Properties.LastModified,
			Size:    *blobInfo.Properties.ContentLength,
			MD5:     blobInfo.Properties.ContentMD5,
			IsDir:   false,
			AsFunc: func(i interface{}) bool {
				p, ok := i.(*azblob.BlobItemInternal)
				if !ok {
					return false
				}
				*p = *blobInfo
				return true
			},
		})
	}
	if resp.NextMarker != nil {
		page.NextPageToken = []byte(*resp.NextMarker)
	}
	if len(segment.BlobPrefixes) > 0 && len(segment.BlobItems) > 0 {
		sort.Slice(page.Objects, func(i, j int) bool {
			return page.Objects[i].Key < page.Objects[j].Key
		})
	}
	return page, nil
}

// SignedURL implements driver.SignedURL.
func (b *bucket) SignedURL(ctx context.Context, key string, opts *driver.SignedURLOptions) (string, error) {
	if opts.ContentType != "" || opts.EnforceAbsentContentType {
		return "", gcerr.New(gcerr.Unimplemented, nil, 1, "azureblob: does not enforce Content-Type on PUT")
	}

	key = escapeKey(key, false)
	blobClient, err := b.client.NewBlobClient(key)
	if err != nil {
		return "", err
	}

	perms := azblob.BlobSASPermissions{}
	switch opts.Method {
	case http.MethodGet:
		perms.Read = true
	case http.MethodPut:
		perms.Create = true
		perms.Write = true
	case http.MethodDelete:
		perms.Delete = true
	default:
		return "", fmt.Errorf("unsupported Method %s", opts.Method)
	}

	if opts.BeforeSign != nil {
		asFunc := func(i interface{}) bool {
			v, ok := i.(**azblob.BlobSASPermissions)
			if ok {
				*v = &perms
			}
			return ok
		}
		if err := opts.BeforeSign(asFunc); err != nil {
			return "", err
		}
	}
	start := time.Now().UTC()
	expiry := start.Add(opts.Expiry)
	sasQueryParams, err := blobClient.GetSASToken(perms, start, expiry)
	sasURL := fmt.Sprintf("%s?%s", blobClient.URL(), sasQueryParams.Encode())
	return sasURL, nil
}

type writer struct {
	ctx        context.Context
	client     *azblob.BlockBlobClient
	uploadOpts *azblob.UploadStreamOptions

	w     *io.PipeWriter
	donec chan struct{}
	err   error
}

// escapeKey does all required escaping for UTF-8 strings to work with Azure.
// isPrefix indicates whether the  key is a full key, or a prefix/delimiter.
func escapeKey(key string, isPrefix bool) string {
	return escape.HexEscape(key, func(r []rune, i int) bool {
		c := r[i]
		switch {
		// Azure does not work well with backslashes in blob names.
		case c == '\\':
			return true
		// Azure doesn't handle these characters (determined via experimentation).
		case c < 32 || c == 127:
			return true
			// Escape trailing "/" for full keys, otherwise Azure can't address them
			// consistently.
		case !isPrefix && i == len(key)-1 && c == '/':
			return true
		// For "../", escape the trailing slash.
		case i > 1 && r[i] == '/' && r[i-1] == '.' && r[i-2] == '.':
			return true
		}
		return false
	})
}

// unescapeKey reverses escapeKey.
func unescapeKey(key string) string {
	return escape.HexUnescape(key)
}

// NewTypedWriter implements driver.NewTypedWriter.
func (b *bucket) NewTypedWriter(ctx context.Context, key string, contentType string, opts *driver.WriterOptions) (driver.Writer, error) {
	key = escapeKey(key, false)
	blobClient, err := b.client.NewBlockBlobClient(key)
	if err != nil {
		return nil, err
	}
	if opts.BufferSize == 0 {
		opts.BufferSize = defaultUploadBlockSize
	}
	if opts.MaxConcurrency == 0 {
		opts.MaxConcurrency = defaultUploadBuffers
	}

	md := make(map[string]string, len(opts.Metadata))
	for k, v := range opts.Metadata {
		// See the package comments for more details on escaping of metadata
		// keys & values.
		e := escape.HexEscape(k, func(runes []rune, i int) bool {
			c := runes[i]
			switch {
			case i == 0 && c >= '0' && c <= '9':
				return true
			case escape.IsASCIIAlphanumeric(c):
				return false
			case c == '_':
				return false
			}
			return true
		})
		if _, ok := md[e]; ok {
			return nil, fmt.Errorf("duplicate keys after escaping: %q => %q", k, e)
		}
		md[e] = escape.URLEscape(v)
	}
	uploadOpts := &azblob.UploadStreamOptions{
		BufferSize: opts.BufferSize,
		MaxBuffers: opts.MaxConcurrency,
		Metadata:   md,
		HTTPHeaders: &azblob.BlobHTTPHeaders{
			BlobCacheControl:       &opts.CacheControl,
			BlobContentDisposition: &opts.ContentDisposition,
			BlobContentEncoding:    &opts.ContentEncoding,
			BlobContentLanguage:    &opts.ContentLanguage,
			BlobContentMD5:         opts.ContentMD5,
			BlobContentType:        &contentType,
		},
	}
	if opts.BeforeWrite != nil {
		asFunc := func(i interface{}) bool {
			p, ok := i.(**azblob.UploadStreamOptions)
			if !ok {
				return false
			}
			*p = uploadOpts
			return true
		}
		if err := opts.BeforeWrite(asFunc); err != nil {
			return nil, err
		}
	}
	return &writer{
		ctx:        ctx,
		client:     blobClient,
		uploadOpts: uploadOpts,
		donec:      make(chan struct{}),
	}, nil
}

// Write appends p to w. User must call Close to close the w after done writing.
func (w *writer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if w.w == nil {
		pr, pw := io.Pipe()
		w.w = pw
		if err := w.open(pr); err != nil {
			return 0, err
		}
	}
	return w.w.Write(p)
}

func (w *writer) open(pr *io.PipeReader) error {
	go func() {
		defer close(w.donec)

		var body io.Reader
		if pr == nil {
			body = http.NoBody
		} else {
			body = pr
		}
		_, w.err = w.client.UploadStream(w.ctx, body, *w.uploadOpts)
		if w.err != nil {
			if pr != nil {
				pr.CloseWithError(w.err)
			}
			return
		}
	}()
	return nil
}

// Close completes the writer and closes it. Any error occurring during write will
// be returned. If a writer is closed before any Write is called, Close will
// create an empty file at the given key.
func (w *writer) Close() error {
	if w.w == nil {
		w.open(nil)
	} else if err := w.w.Close(); err != nil {
		return err
	}
	<-w.donec
	return w.err
}
