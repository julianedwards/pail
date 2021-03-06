package pail

import (
	"context"
	"io"
)

// Bucket defines an interface for accessing a remote blob store, like
// S3. Should be generic enough to be implemented for GCP equivalent,
// or even a GridFS backed system (mostly just for kicks.)
//
// Other goals of this project are to allow us to have a single
// interface for interacting with blob storage, and allow us to fully
// move off of our legacy goamz package and stabalize all blob-storage
// operations across all projects. There should be no interface
// dependencies on external packages required to use this library.
//
// See, the following implemenations for previous approaches.
//
//   - https://github.com/evergreen-ci/evergreen/blob/master/thirdparty/s3.go
//   - https://github.com/mongodb/curator/tree/master/sthree
//
// The prefered aws sdk is here: https://docs.aws.amazon.com/sdk-for-go/api/
//
// In no particular order:
//  - implementation constructors should make it possible to use
//    custom http.Clients (to aid in pooling.)
//  - We should probably implement .String methods.
//  - Do use the grip package for logging.
//  - get/put should support multipart upload/download?
//  - we'll want to do retries with back-off (potentially configurable
//    in bucketinfo?)
//  - we might need to have variants that Put/Get byte slices rather
//    than readers.
//  - pass contexts to requests for timeouts.
//
// Eventually we'll move this package to its own repository, but for
// now we can do development here.

type Bucket interface {
	Check(context.Context) error

	// Produces a Writer and Reader interface to the file named by
	// the string.
	Writer(context.Context, string) (io.WriteCloser, error)
	Reader(context.Context, string) (io.ReadCloser, error)

	// Put and Get write simple byte streams (in the form of
	// io.Readers) to/from specfied keys.
	//
	// TODOD: consider if these, particularly Get are not
	// substantively different from Writer/Reader methods, or
	// might just be a wrapper.
	Put(context.Context, string, io.Reader) error
	Get(context.Context, string) (io.ReadCloser, error)

	// Upload and Download write files from the local file
	// system to the specified key.
	Upload(context.Context, string, string) error
	Download(context.Context, string, string) error

	// Sync methods: these methods are the recursive, efficient
	// copy methods of files from s3 to the local file
	// system.
	Push(context.Context, string, string) error
	Pull(context.Context, string, string) error

	// Copy does a special copy operation that does not require
	// downloading a file. Note that CopyOptions.DestinationBucket must
	// have the same type as the calling bucket object.
	Copy(context.Context, CopyOptions) error

	// Remove the specified object from the bucket.
	Remove(context.Context, string) error

	// List provides a way to iterator over the contents of a
	// bucket (for a given prefix.)
	List(context.Context, string) (BucketIterator, error)
}

type CopyOptions struct {
	SourceKey         string
	DestinationKey    string
	DestinationBucket Bucket
	IsDestination     bool
}

////////////////////////////////////////////////////////////////////////
//
// Iterator

// While iterators (typically) use channels internally, this is a
// fairly standard paradigm for iterating through resources, and is
// use heavily in the FTDC library (https://github.com/mongodb/ftdc)
// and bson (https://godoc.org/github.com/mongodb/mongo-go-driver/bson)
// libraries.

type BucketIterator interface {
	Next(context.Context) bool
	Err() error
	Item() BucketItem
}

type BucketItem interface {
	Bucket() string
	Name() string
	Hash() string
	Get(context.Context) (io.ReadCloser, error)
}

type bucketItemImpl struct {
	bucket string
	key    string
	hash   string

	// TODO add other info?

	// QUESTION: does this need to be an interface to support
	// additional information?

	b Bucket
}

func (bi *bucketItemImpl) Name() string   { return bi.key }
func (bi *bucketItemImpl) Hash() string   { return bi.hash }
func (bi *bucketItemImpl) Bucket() string { return bi.bucket }
func (bi *bucketItemImpl) Get(ctx context.Context) (io.ReadCloser, error) {
	return bi.b.Get(ctx, bi.key)
}
