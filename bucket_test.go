package pail

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mongodb/grip"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	mgo "gopkg.in/mgo.v2"
)

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func createS3Client(region string) (*s3.S3, error) {
	sess, err := session.NewSession(&aws.Config{Region: aws.String(region)})
	if err != nil {
		return nil, errors.Wrap(err, "problem connecting to AWS")
	}
	svc := s3.New(sess)
	return svc, nil
}

func cleanUpS3Bucket(name, prefix, region string) error {
	svc, err := createS3Client(region)
	if err != nil {
		return errors.Wrap(err, "clean up failed")
	}
	doi := &s3.DeleteObjectsInput{
		Bucket: aws.String(name),
		Delete: &s3.Delete{},
	}
	catcher := grip.NewCatcher()
	var result *s3.ListObjectsOutput
	for {
		listInput := &s3.ListObjectsInput{
			Bucket: aws.String(name),
		}
		result, err = svc.ListObjects(listInput)
		if err != nil {
			catcher.Add(errors.Wrap(err, "clean up failed"))
			continue
		}
		for _, object := range result.Contents {
			if !strings.HasPrefix(*object.Key, prefix) {
				continue
			}
			doi.Delete.Objects = append(doi.Delete.Objects, &s3.ObjectIdentifier{
				Key: object.Key,
			})

		}
		if !*result.IsTruncated {
			break
		}
	}

	if catcher.HasErrors() {
		return catcher.Resolve()
	}

	_, err = svc.DeleteObjects(doi)
	if err != nil {
		return errors.Wrap(err, "failed to delete S3 bucket")
	}

	return nil
}

func TestBucket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	uuid := newUUID()
	_, file, _, _ := runtime.Caller(0)
	tempdir, err := ioutil.TempDir("", "pail-bucket-test")
	require.NoError(t, err)
	defer func() { require.NoError(t, os.RemoveAll(tempdir)) }()
	require.NoError(t, err, os.MkdirAll(filepath.Join(tempdir, uuid), 0700))

	ses, err := mgo.DialWithTimeout("mongodb://localhost:27017", time.Second)
	require.NoError(t, err)
	defer ses.Close()
	defer func() { ses.DB(uuid).DropDatabase() }()

	s3BucketName := "build-test-curator"
	s3Prefix := "pail-test-"
	s3Region := "us-east-1"
	defer func() { require.NoError(t, cleanUpS3Bucket(s3BucketName, s3Prefix, s3Region)) }()

	type bucketTestCase struct {
		id   string
		test func(*testing.T, Bucket)
	}

	for _, impl := range []struct {
		name        string
		constructor func(*testing.T) Bucket
		tests       []bucketTestCase
	}{
		{
			name: "Local",
			constructor: func(t *testing.T) Bucket {
				path := filepath.Join(tempdir, uuid, newUUID())
				require.NoError(t, os.MkdirAll(path, 0777))
				return &localFileSystem{path: path}
			},
			tests: []bucketTestCase{
				{
					id: "VerifyBucketType",
					test: func(t *testing.T, b Bucket) {
						bucket, ok := b.(*localFileSystem)
						require.True(t, ok)
						assert.NotNil(t, bucket)
					},
				},
				{
					id: "PathDoesNotExist",
					test: func(t *testing.T, b Bucket) {
						bucket := b.(*localFileSystem)
						bucket.path = "foo"
						assert.Error(t, bucket.Check(ctx))
					},
				},
				{
					id: "WriterErrorFileName",
					test: func(t *testing.T, b Bucket) {
						_, err := b.Writer(ctx, "\x00")
						require.Error(t, err)
						assert.Contains(t, err.Error(), "problem opening")
					},
				},
				{
					id: "ReaderErrorFileName",
					test: func(t *testing.T, b Bucket) {
						_, err := b.Reader(ctx, "\x00")
						require.Error(t, err)
						assert.Contains(t, err.Error(), "problem opening")
					},
				},
				{
					id: "CopyErrorFileNameFrom",
					test: func(t *testing.T, b Bucket) {
						options := CopyOptions{
							SourceKey:         "\x00",
							DestinationKey:    "foo",
							DestinationBucket: b,
						}
						err := b.Copy(ctx, options)
						require.Error(t, err)
						assert.Contains(t, err.Error(), "problem opening")
					},
				},
				{
					id: "CopyErrorFileNameTo",
					test: func(t *testing.T, b Bucket) {
						fn := filepath.Base(file)
						err := b.Upload(ctx, "foo", fn)
						require.NoError(t, err)

						options := CopyOptions{
							SourceKey:         "foo",
							DestinationKey:    "\x00",
							DestinationBucket: b,
						}
						err = b.Copy(ctx, options)
						require.Error(t, err)
						assert.Contains(t, err.Error(), "problem opening")
					},
				},
				{
					id: "PutErrorFileName",
					test: func(t *testing.T, b Bucket) {
						err := b.Put(ctx, "\x00", nil)
						require.Error(t, err)
						assert.Contains(t, err.Error(), "problem opening")
					},
				},
				{
					id: "PutErrorReader",
					test: func(t *testing.T, b Bucket) {
						err := b.Put(ctx, "foo", &brokenWriter{})
						require.Error(t, err)
						assert.Contains(t, err.Error(), "problem copying data to file")
					},
				},
				{
					id: "WriterErrorDirectoryName",
					test: func(t *testing.T, b Bucket) {
						bucket := b.(*localFileSystem)
						bucket.path = "\x00"
						_, err := b.Writer(ctx, "foo")
						require.Error(t, err)
						assert.Contains(t, err.Error(), "problem creating base directories")
					},
				},
				{
					id: "PullErrorsContext",
					test: func(t *testing.T, b Bucket) {
						tctx, cancel := context.WithCancel(ctx)
						cancel()
						bucket := b.(*localFileSystem)
						bucket.path = ""
						err := b.Pull(tctx, "", filepath.Dir(file))
						assert.Error(t, err)
					},
				},
				{
					id: "PushErrorsContext",
					test: func(t *testing.T, b Bucket) {
						tctx, cancel := context.WithCancel(ctx)
						cancel()
						err := b.Push(tctx, filepath.Dir(file), "")
						assert.Error(t, err)
					},
				},
			},
		},
		{
			name: "LegacyGridFS",
			constructor: func(t *testing.T) Bucket {
				b, err := NewLegacyGridFSBucketWithSession(ses.Clone(), GridFSOptions{
					Prefix:   newUUID(),
					Database: uuid,
				})
				require.NoError(t, err)
				return b
			},
			tests: []bucketTestCase{
				{
					id: "VerifyBucketType",
					test: func(t *testing.T, b Bucket) {
						bucket, ok := b.(*gridfsLegacyBucket)
						require.True(t, ok)
						assert.NotNil(t, bucket)
					},
				},
				{
					id: "OpenFailsWithClosedSession",
					test: func(t *testing.T, b Bucket) {
						bucket := b.(*gridfsLegacyBucket)
						go func() {
							time.Sleep(time.Millisecond)
							bucket.session.Close()
						}()
						_, err := bucket.openFile(ctx, "foo", false)
						assert.Error(t, err)
					},
				},
			},
		},
		{
			name: "S3Bucket",
			constructor: func(t *testing.T) Bucket {
				s3Options := S3Options{
					Region: s3Region,
					Name:   s3BucketName,
					Prefix: s3Prefix + newUUID(),
				}
				b, err := NewS3Bucket(s3Options)
				require.NoError(t, err)
				return b
			},
			tests: []bucketTestCase{
				{
					id: "VerifyBucketType",
					test: func(t *testing.T, b Bucket) {
						bucket, ok := b.(*s3BucketSmall)
						require.True(t, ok)
						assert.NotNil(t, bucket)
					},
				},
				{
					id: "TestCredentialsOverrideDefaults",
					test: func(t *testing.T, b Bucket) {
						assert.NoError(t, b.Check(ctx))
						badOptions := S3Options{
							Credentials: credentials.NewStaticCredentials("asdf", "asdf", "asdf"),
							Region:      s3Region,
							Name:        s3BucketName,
						}
						badBucket, err := NewS3Bucket(badOptions)
						assert.Nil(t, err)
						assert.Error(t, badBucket.Check(ctx))
					},
				},
			},
		},
		{
			name: "S3MultiPartBucket",
			constructor: func(t *testing.T) Bucket {
				s3Options := S3Options{
					Region: s3Region,
					Name:   s3BucketName,
					Prefix: s3Prefix + newUUID(),
				}
				b, err := NewS3MultiPartBucket(s3Options)
				require.NoError(t, err)
				return b
			},
			tests: []bucketTestCase{
				{
					id: "VerifyBucketType",
					test: func(t *testing.T, b Bucket) {
						bucket, ok := b.(*s3BucketLarge)
						require.True(t, ok)
						assert.NotNil(t, bucket)
					},
				},
			},
		},
	} {
		t.Run(impl.name, func(t *testing.T) {
			for _, test := range impl.tests {
				t.Run(test.id, func(t *testing.T) {
					bucket := impl.constructor(t)
					test.test(t, bucket)
				})
			}
			t.Run("ValidateFixture", func(t *testing.T) {
				assert.NotNil(t, impl.constructor(t))
			})
			t.Run("CheckIsValid", func(t *testing.T) {
				assert.NoError(t, impl.constructor(t).Check(ctx))
			})
			t.Run("ListIsEmpty", func(t *testing.T) {
				bucket := impl.constructor(t)
				iter, err := bucket.List(ctx, "")
				require.NoError(t, err)
				assert.False(t, iter.Next(ctx))
				assert.Nil(t, iter.Item())
				assert.NoError(t, iter.Err())
			})
			t.Run("ListErrorsWithCancledContext", func(t *testing.T) {
				bucket := impl.constructor(t)
				tctx, cancel := context.WithCancel(ctx)
				cancel()
				iter, err := bucket.List(tctx, "")
				assert.Error(t, err)
				assert.Nil(t, iter)
			})
			t.Run("WriteOneFile", func(t *testing.T) {
				bucket := impl.constructor(t)
				key := newUUID()
				assert.NoError(t, writeDataToFile(ctx, bucket, key, "hello world!"))

				// just check that it exists in the iterator
				iter, err := bucket.List(ctx, "")
				require.NoError(t, err)
				assert.True(t, iter.Next(ctx))
				assert.False(t, iter.Next(ctx))
				assert.NoError(t, iter.Err())
			})

			t.Run("RemoveOneFile", func(t *testing.T) {
				bucket := impl.constructor(t)
				key := newUUID()
				assert.NoError(t, writeDataToFile(ctx, bucket, key, "hello world!"))

				// just check that it exists in the iterator
				iter, err := bucket.List(ctx, "")
				require.NoError(t, err)
				assert.True(t, iter.Next(ctx))
				assert.False(t, iter.Next(ctx))
				assert.NoError(t, iter.Err())

				assert.NoError(t, bucket.Remove(ctx, key))
				iter, err = bucket.List(ctx, "")
				require.NoError(t, err)
				assert.False(t, iter.Next(ctx))
				assert.Nil(t, iter.Item())
				assert.NoError(t, iter.Err())
			})
			t.Run("ReadWriteRoundTripSimple", func(t *testing.T) {
				bucket := impl.constructor(t)
				key := newUUID()
				payload := "hello world!"
				require.NoError(t, writeDataToFile(ctx, bucket, key, payload))

				data, err := readDataFromFile(ctx, bucket, key)
				assert.NoError(t, err)
				assert.Equal(t, data, payload)
			})
			t.Run("GetRetrievesData", func(t *testing.T) {
				bucket := impl.constructor(t)
				key := newUUID()
				assert.NoError(t, writeDataToFile(ctx, bucket, key, "hello world!"))

				reader, err := bucket.Get(ctx, key)
				require.NoError(t, err)
				data, err := ioutil.ReadAll(reader)
				require.NoError(t, err)
				assert.Equal(t, "hello world!", string(data))
			})
			t.Run("PutSavesFiles", func(t *testing.T) {
				const contents = "check data"
				bucket := impl.constructor(t)
				key := newUUID()

				assert.NoError(t, bucket.Put(ctx, key, bytes.NewBuffer([]byte(contents))))

				reader, err := bucket.Get(ctx, key)
				require.NoError(t, err)
				data, err := ioutil.ReadAll(reader)
				require.NoError(t, err)
				assert.Equal(t, contents, string(data))
			})
			t.Run("CopyDuplicatesData", func(t *testing.T) {
				const contents = "this one"
				bucket := impl.constructor(t)
				keyOne := newUUID()
				keyTwo := newUUID()
				assert.NoError(t, writeDataToFile(ctx, bucket, keyOne, contents))
				options := CopyOptions{
					SourceKey:         keyOne,
					DestinationKey:    keyTwo,
					DestinationBucket: bucket,
				}
				assert.NoError(t, bucket.Copy(ctx, options))
				data, err := readDataFromFile(ctx, bucket, keyTwo)
				require.NoError(t, err)
				assert.Equal(t, contents, data)
			})
			t.Run("CopyDuplicatesToDifferentBucket", func(t *testing.T) {
				const contents = "this one"
				srcBucket := impl.constructor(t)
				destBucket := impl.constructor(t)
				keyOne := newUUID()
				keyTwo := newUUID()
				assert.NoError(t, writeDataToFile(ctx, srcBucket, keyOne, contents))
				options := CopyOptions{
					SourceKey:         keyOne,
					DestinationKey:    keyTwo,
					DestinationBucket: destBucket,
				}
				assert.NoError(t, srcBucket.Copy(ctx, options))
				data, err := readDataFromFile(ctx, destBucket, keyTwo)
				require.NoError(t, err)
				assert.Equal(t, contents, data)
			})
			t.Run("DownloadWritesFileToDisk", func(t *testing.T) {
				const contents = "in the file"
				bucket := impl.constructor(t)
				key := newUUID()
				path := filepath.Join(tempdir, uuid, key)

				assert.NoError(t, writeDataToFile(ctx, bucket, key, contents))

				_, err := os.Stat(path)
				assert.True(t, os.IsNotExist(err))
				assert.NoError(t, bucket.Download(ctx, key, path))
				_, err = os.Stat(path)
				assert.False(t, os.IsNotExist(err))

				data, err := ioutil.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, contents, string(data))
			})
			t.Run("ListRespectsPrefixes", func(t *testing.T) {
				bucket := impl.constructor(t)
				key := newUUID()

				assert.NoError(t, writeDataToFile(ctx, bucket, key, "foo/bar"))

				// there's one thing in the iterator
				// with the correct prefix
				iter, err := bucket.List(ctx, "")
				require.NoError(t, err)
				assert.True(t, iter.Next(ctx))
				assert.False(t, iter.Next(ctx))
				assert.NoError(t, iter.Err())

				// there's nothing in the iterator
				// with a prefix
				iter, err = bucket.List(ctx, "bar")
				require.NoError(t, err)
				assert.False(t, iter.Next(ctx))
				assert.Nil(t, iter.Item())
				assert.NoError(t, iter.Err())
			})
			t.Run("RoundTripManyFiles", func(t *testing.T) {
				data := map[string]string{}
				for i := 0; i < 300; i++ {
					data[newUUID()] = strings.Join([]string{newUUID(), newUUID(), newUUID()}, "\n")
				}

				bucket := impl.constructor(t)
				for k, v := range data {
					assert.NoError(t, writeDataToFile(ctx, bucket, k, v))
				}

				iter, err := bucket.List(ctx, "")
				require.NoError(t, err)
				count := 0
				for iter.Next(ctx) {
					count++
					item := iter.Item()
					require.NotNil(t, item)

					key := item.Name()
					_, ok := data[key]
					require.True(t, ok)
					assert.NotZero(t, item.Bucket())

					reader, err := item.Get(ctx)
					require.NoError(t, err)
					require.NotNil(t, reader)
					out, err := ioutil.ReadAll(reader)
					assert.NoError(t, err)
					assert.NoError(t, reader.Close())
					assert.Equal(t, string(out), data[item.Name()])
				}
				assert.Equal(t, 300, count)
				assert.NoError(t, iter.Err())
			})
			t.Run("PullFromBucket", func(t *testing.T) {
				data := map[string]string{}
				for i := 0; i < 100; i++ {
					data[newUUID()] = strings.Join([]string{newUUID(), newUUID(), newUUID()}, "\n")
				}

				bucket := impl.constructor(t)
				for k, v := range data {
					assert.NoError(t, writeDataToFile(ctx, bucket, k, v))
				}

				mirror := filepath.Join(tempdir, "pull-one", newUUID())
				require.NoError(t, os.MkdirAll(mirror, 0700))
				for i := 0; i < 3; i++ {
					assert.NoError(t, bucket.Pull(ctx, mirror, ""))
					files, err := walkLocalTree(ctx, mirror)
					require.NoError(t, err)
					assert.Len(t, files, 100)

					if impl.name != "LegacyGridFS" {
						for _, fn := range files {
							_, ok := data[filepath.Base(fn)]
							assert.True(t, ok)
						}
					}
				}

			})
			t.Run("PushToBucket", func(t *testing.T) {
				prefix := filepath.Join(tempdir, newUUID())
				for i := 0; i < 100; i++ {
					require.NoError(t, writeDataToDisk(prefix,
						newUUID(), strings.Join([]string{newUUID(), newUUID(), newUUID()}, "\n")))
				}

				bucket := impl.constructor(t)
				t.Run("NoPrefix", func(t *testing.T) {
					assert.NoError(t, bucket.Push(ctx, prefix, ""))
					assert.NoError(t, bucket.Push(ctx, prefix, ""))
				})
				t.Run("ShortPrefix", func(t *testing.T) {
					assert.NoError(t, bucket.Push(ctx, prefix, "foo"))
					assert.NoError(t, bucket.Push(ctx, prefix, "foo"))
				})
				t.Run("BucketContents", func(t *testing.T) {
					iter, err := bucket.List(ctx, "")
					require.NoError(t, err)
					counter := 0
					for iter.Next(ctx) {
						counter++
					}
					assert.NoError(t, iter.Err())
					assert.Equal(t, 200, counter)
				})
			})
			t.Run("UploadWithBadFileName", func(t *testing.T) {
				bucket := impl.constructor(t)
				err := bucket.Upload(ctx, "key", "foo\x00bar")
				require.Error(t, err)
				assert.Contains(t, err.Error(), "problem opening file")
			})
			t.Run("DownloadWithBadFileName", func(t *testing.T) {
				bucket := impl.constructor(t)
				err := bucket.Download(ctx, "fileIWant\x00", "loc")
				assert.Error(t, err)
			})
			t.Run("DownloadBadDirectory", func(t *testing.T) {
				bucket := impl.constructor(t)
				fn := filepath.Base(file)
				err := bucket.Upload(ctx, "key", fn)
				require.NoError(t, err)

				err = bucket.Download(ctx, "key", "location-\x00/key-name")
				require.Error(t, err)
				assert.Contains(t, err.Error(), "problem creating enclosing directory")
			})
			t.Run("DownloadToBadFileName", func(t *testing.T) {
				bucket := impl.constructor(t)
				fn := filepath.Base(file)
				err := bucket.Upload(ctx, "key", fn)
				require.NoError(t, err)

				err = bucket.Download(ctx, "key", "location-\x00-key-name")
				require.Error(t, err)
				assert.Contains(t, err.Error(), "problem creating file")

			})
		})
	}
}

func writeDataToDisk(prefix, key, data string) error {
	if err := os.MkdirAll(prefix, 0700); err != nil {
		return errors.WithStack(err)
	}
	path := filepath.Join(prefix, key)
	return errors.WithStack(ioutil.WriteFile(path, []byte(data), 0600))
}

func writeDataToFile(ctx context.Context, bucket Bucket, key, data string) error {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	writer, err := bucket.Writer(wctx, key)
	if err != nil {
		return errors.WithStack(err)
	}

	_, err = writer.Write([]byte(data))
	if err != nil {
		return errors.WithStack(err)
	}

	return errors.WithStack(writer.Close())
}

func readDataFromFile(ctx context.Context, bucket Bucket, key string) (string, error) {
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()

	reader, err := bucket.Reader(rctx, key)
	if err != nil {
		return "", errors.WithStack(err)
	}
	out, err := ioutil.ReadAll(reader)
	if err != nil {
		return "", errors.WithStack(err)
	}

	err = reader.Close()
	if err != nil {
		return "", errors.WithStack(err)
	}

	return string(out), nil

}

type brokenWriter struct{}

func (*brokenWriter) Write(_ []byte) (int, error) { return -1, errors.New("always") }
func (*brokenWriter) Read(_ []byte) (int, error)  { return -1, errors.New("always") }
