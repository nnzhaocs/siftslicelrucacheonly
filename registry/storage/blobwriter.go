package storage

import (
	"errors"
	"fmt"
	"io"
	"path"
	"time"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/docker/distribution"
	"github.com/docker/distribution/context"
	storagedriver "github.com/docker/distribution/registry/storage/driver"
	"github.com/opencontainers/go-digest"
//NANNAN	
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/idtools"
	"path/filepath"
	"github.com/docker/distribution/registry/storage/cache"
	"os"

//	"ioutil"
	
)

//NANNAN: TODO LIST
//1. when storing to recipe, remove prefix-:/var/lib/registry/docker/registry/v2/blobs/sha256/ for redis space savings.

var (
	errResumableDigestNotAvailable = errors.New("resumable digest not available")
	//NANNAN
	algorithm   = digest.Canonical
)

const (
	// digestSha256Empty is the canonical sha256 digest of empty data
	digestSha256Empty = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// blobWriter is used to control the various aspects of resumable
// blob upload.
type blobWriter struct {
	ctx       context.Context
	blobStore *linkedBlobStore

	id        string
	startedAt time.Time
	digester  digest.Digester
	written   int64 // track the contiguous write
	//NANNAN: filewriter
	fileWriter storagedriver.FileWriter
	driver     storagedriver.StorageDriver
	path       string

	resumableDigestEnabled bool
	committed              bool
}

var _ distribution.BlobWriter = &blobWriter{}

// ID returns the identifier for this upload.
func (bw *blobWriter) ID() string {
	return bw.id
}

func (bw *blobWriter) StartedAt() time.Time {
	return bw.startedAt
}

// Commit marks the upload as completed, returning a valid descriptor. The
// final size and digest are checked against the first descriptor provided.
func (bw *blobWriter) Commit(ctx context.Context, desc distribution.Descriptor) (distribution.Descriptor, error) {
	context.GetLogger(ctx).Debug("(*blobWriter).Commit")

	if err := bw.fileWriter.Commit(); err != nil {
		return distribution.Descriptor{}, err
	}

	bw.Close()
	desc.Size = bw.Size()

	canonical, err := bw.validateBlob(ctx, desc)
	if err != nil {
		return distribution.Descriptor{}, err
	}

	if err := bw.moveBlob(ctx, canonical); err != nil {
		return distribution.Descriptor{}, err
	}

	if err := bw.blobStore.linkBlob(ctx, canonical, desc.Digest); err != nil {
		return distribution.Descriptor{}, err
	}

	if err := bw.removeResources(ctx); err != nil {
		return distribution.Descriptor{}, err
	}

	err = bw.blobStore.blobAccessController.SetDescriptor(ctx, canonical.Digest, canonical)
	if err != nil {
		return distribution.Descriptor{}, err
	}

	bw.committed = true
	return canonical, nil
}

//NANNAN: utility function. to remove duplicate ips from serverips
func RemoveDuplicateIpsFromIps(s []string) []string {
      m := make(map[string]bool)
      for _, item := range s {
              if _, ok := m[item]; ok {
                      // duplicate item
                      fmt.Println(item, "is a duplicate")
              } else {
                      m[item] = true
              }
      }

      var result []string
      for item, _ := range m {
              result = append(result, item)
      }
      return result
}

//NANNAN: after finishing commit, start do deduplication
//TODO delete tarball
//type BFmap map[digest.Digest][]distribution.FileDescriptor

func (bw *blobWriter) Dedup(ctx context.Context, desc distribution.Descriptor) (error) {
	
	context.GetLogger(ctx).Debug("NANNAN: (*blobWriter).Dedup")

	blobPath, err := PathFor(BlobDataPathSpec{
		Digest: desc.Digest,
	})
	context.GetLogger(ctx).Debugf("NANNAN: blob = %v:%v", blobPath, desc.Digest)
	
	_, err = bw.blobStore.registry.fileDescriptorCacheProvider.StatBFRecipe(ctx, desc.Digest)
	if err == nil{
		context.GetLogger(ctx).Debug("NANNAN: THIS LAYER TARBALL ALREADY DEDUPED :=>%v", desc.Digest)
		return nil
	}
	
	//DedupLayersFromPath(blobPath)
	//log.Warnf("IBM: HTTP GET: %s", dgst)
	//WithField("digest", desc.Digest).Warnf("attempted to move zero-length content with non-zero digest")
	
	layerPath := path.Join("/var/lib/registry", blobPath)
	
	context.GetLogger(ctx).Debug("NANNAN: START DEDUPLICATION FROM PATH :=>%s", layerPath)

	parentDir := path.Dir(layerPath)
	unpackPath := path.Join(parentDir, "diff")

	archiver := archive.NewDefaultArchiver()
	options := &archive.TarOptions{
		UIDMaps: archiver.IDMapping.UIDs(),
		GIDMaps: archiver.IDMapping.GIDs(),
	}
	idMapping := idtools.NewIDMappingsFromMaps(options.UIDMaps, options.GIDMaps)
	rootIDs := idMapping.RootPair()
	err = idtools.MkdirAllAndChownNew(unpackPath, 0777, rootIDs)
	if err != nil {
		context.GetLogger(ctx).Errorf("NANNAN: %s", err)
		return err
	}

	err = archiver.UntarPath(layerPath, unpackPath)
	if err != nil {
		//TODO: process manifest file
		context.GetLogger(ctx).Errorf("NANNAN: %s, This may be a manifest file", err)
		return err
	}
	
	var bfdescriptors [] distribution.BFDescriptor
	var serverIps []string
	
	err = filepath.Walk(unpackPath, bw.CheckDuplicate(ctx, bw.blobStore.registry.serverIp, desc, bw.blobStore.registry.fileDescriptorCacheProvider, &bfdescriptors, &serverIps))
	if err != nil {
		context.GetLogger(ctx).Errorf("NANNAN: %s", err)
	}
	
	serverIps = append(serverIps, bw.blobStore.registry.serverIp) //NANNAN add this serverip
	
	des := distribution.BFRecipeDescriptor{
		BlobDigest: desc.Digest,
		BFDescriptors: bfdescriptors,
		ServerIps: RemoveDuplicateIpsFromIps(serverIps),
	}
	context.GetLogger(ctx).Debug("NANNAN: %v", des)
	err = bw.blobStore.registry.fileDescriptorCacheProvider.SetBFRecipe(ctx, desc.Digest, des)
	if err != nil {
		return err
	}
	return err
}

//NANNAN check dedup
// Metrics: lock

func (bw *blobWriter) CheckDuplicate(ctx context.Context, serverIp string, desc distribution.Descriptor, db cache.FileDescriptorCacheProvider, bfdescriptors *[] distribution.BFDescriptor, serverIps *[] string) filepath.WalkFunc {
//	totalFiles := 0
//	sameFiles := 0
//	reguFiles := 0
//	rmFiles := 0
	
	return func(fpath string, info os.FileInfo, err error) error {
//		context.GetLogger(ctx).Debug("NANNAN: START CHECK DUPLICATES :=>")

		if err != nil {
			context.GetLogger(ctx).Errorf("NANNAN: ", err)
			return err
		}
//		context.GetLogger(ctx).Debug("NANNAN: totalFiles,sameFiles,reguFiles,rmFiles", totalFiles, sameFiles, reguFiles, rmFiles)
//		if info.IsDir() {
//			context.GetLogger(ctx).Debug("NANNAN: TODO process directories")
//			
//			return nil
//		}
		
		//NANNAN: CHECK file stat, skip symlink and hardlink
//		totalFiles = totalFiles + 1
		
		if ! (info.Mode().IsRegular()){
//			context.GetLogger(ctx).Debug("NANNAN: TODO process sysmlink and othrs")
			return nil
		}
		
//		reguFiles = reguFiles + 1
			
		fp, err := os.Open(fpath) 
		if err != nil {
			context.GetLogger(ctx).Errorf("NANNAN: %s", err)
			return nil
		}
		
		defer fp.Close()
		
		digestFn := algorithm.FromReader
		dgst, err := digestFn(fp)
		if err != nil {
			context.GetLogger(ctx).Errorf("NANNAN: %s: %v", fpath, err)
			return err
		}
		
		//NANNAN: add file to map	

		// var serverIps []string
		des, err := db.StatFile(ctx, dgst)
		if err == nil {
			// file content already present	
			//first update layer metadata
			//delete this file
//			sameFiles = sameFiles + 1
			//if fpath == des.FilePath{
			//	context.GetLogger(ctx).Debug("NANNAN: This layer tarball has already deduped: %v!\n", dgst)
			//	return nil
			//}
			err := os.Remove(fpath)
			if err != nil {
			  context.GetLogger(ctx).Errorf("NANNAN: %s", err)
			  return err
			}
//			rmFiles = rmFiles + 1
//			context.GetLogger(ctx).Debug("NANNAN: REMVE file %s", path)
			
			dfp := des.FilePath
			bfdescriptor := distribution.BFDescriptor{
				BlobFilePath: fpath,
				Digest:    dgst,
				DigestFilePath: dfp,
				ServerIp:	des.ServerIp,
			}
			
			*bfdescriptors = append(*bfdescriptors, bfdescriptor)
			*serverIps = append(*serverIps, des.ServerIp)
			
			return nil
		} else if err != distribution.ErrBlobUnknown {
			context.GetLogger(ctx).Errorf("NANNAN: checkDuplicate: error stating content (%v): %v", dgst, err)
			// real error, return it
//			fmt.Println(err)
			return err
			//return distribution.Descriptor{}, err	
		}
		
		//to avoid invalid filepath, rename the original file to digest //tarfpath := strings.SplitN(dgst.String(), ":", 2)[1]
		
		reFPath := path.Join(path.Dir(fpath), strings.SplitN(dgst.String(), ":", 2)[1])
		err = os.Rename(fpath, reFPath)
		if err != nil{
			context.GetLogger(ctx).Errorf("NANNAN: fail to rename path (%v): %v", fpath, reFPath)
			return err
		}
		
		fpath = reFPath
		
	//	var desc distribution.FileDescriptor	
		des = distribution.FileDescriptor{
			
	//		Size: int64(len(p)),
			// NOTE(stevvooe): The central blob store firewalls media types from
			// other users. The caller should look this up and override the value
			// for the specific repository.
			FilePath: fpath,
			Digest:    dgst,
		}
		
		err = db.SetFileDescriptor(ctx, dgst, des)
		if err != nil {
			return err
		}
		
		dfp := des.FilePath
		bfdescriptor := distribution.BFDescriptor{
			BlobFilePath: fpath,
			Digest:    dgst,
			DigestFilePath: dfp,
			ServerIp: serverIp,
		}
				
		*bfdescriptors = append(*bfdescriptors, bfdescriptor)
		
		return nil
	}
	
}


// Cancel the blob upload process, releasing any resources associated with
// the writer and canceling the operation.
func (bw *blobWriter) Cancel(ctx context.Context) error {
	context.GetLogger(ctx).Debug("(*blobWriter).Cancel")
	if err := bw.fileWriter.Cancel(); err != nil {
		return err
	}

	if err := bw.Close(); err != nil {
		context.GetLogger(ctx).Errorf("error closing blobwriter: %s", err)
	}

	if err := bw.removeResources(ctx); err != nil {
		return err
	}

	return nil
}

func (bw *blobWriter) Size() int64 {
	return bw.fileWriter.Size()
}

func (bw *blobWriter) Write(p []byte) (int, error) {
	// Ensure that the current write offset matches how many bytes have been
	// written to the digester. If not, we need to update the digest state to
	// match the current write position.
	if err := bw.resumeDigest(bw.blobStore.ctx); err != nil && err != errResumableDigestNotAvailable {
		return 0, err
	}

	n, err := io.MultiWriter(bw.fileWriter, bw.digester.Hash()).Write(p)
	bw.written += int64(n)

	return n, err
}

func (bw *blobWriter) ReadFrom(r io.Reader) (n int64, err error) {
	// Ensure that the current write offset matches how many bytes have been
	// written to the digester. If not, we need to update the digest state to
	// match the current write position.
	if err := bw.resumeDigest(bw.blobStore.ctx); err != nil && err != errResumableDigestNotAvailable {
		return 0, err
	}

	nn, err := io.Copy(io.MultiWriter(bw.fileWriter, bw.digester.Hash()), r)
	bw.written += nn

	return nn, err
}

func (bw *blobWriter) Close() error {
	if bw.committed {
		return errors.New("blobwriter close after commit")
	}

	if err := bw.storeHashState(bw.blobStore.ctx); err != nil && err != errResumableDigestNotAvailable {
		return err
	}

	return bw.fileWriter.Close()
}

// validateBlob checks the data against the digest, returning an error if it
// does not match. The canonical descriptor is returned.
func (bw *blobWriter) validateBlob(ctx context.Context, desc distribution.Descriptor) (distribution.Descriptor, error) {
	var (
		verified, fullHash bool
		canonical          digest.Digest
	)

	if desc.Digest == "" {
		// if no descriptors are provided, we have nothing to validate
		// against. We don't really want to support this for the registry.
		return distribution.Descriptor{}, distribution.ErrBlobInvalidDigest{
			Reason: fmt.Errorf("cannot validate against empty digest"),
		}
	}

	var size int64

	// Stat the on disk file
	if fi, err := bw.driver.Stat(ctx, bw.path); err != nil {
		switch err := err.(type) {
		case storagedriver.PathNotFoundError:
			// NOTE(stevvooe): We really don't care if the file is
			// not actually present for the reader. We now assume
			// that the desc length is zero.
			desc.Size = 0
		default:
			// Any other error we want propagated up the stack.
			return distribution.Descriptor{}, err
		}
	} else {
		if fi.IsDir() {
			return distribution.Descriptor{}, fmt.Errorf("unexpected directory at upload location %q", bw.path)
		}

		size = fi.Size()
	}

	if desc.Size > 0 {
		if desc.Size != size {
			return distribution.Descriptor{}, distribution.ErrBlobInvalidLength
		}
	} else {
		// if provided 0 or negative length, we can assume caller doesn't know or
		// care about length.
		desc.Size = size
	}

	// TODO(stevvooe): This section is very meandering. Need to be broken down
	// to be a lot more clear.

	if err := bw.resumeDigest(ctx); err == nil {
		canonical = bw.digester.Digest()

		if canonical.Algorithm() == desc.Digest.Algorithm() {
			// Common case: client and server prefer the same canonical digest
			// algorithm - currently SHA256.
			verified = desc.Digest == canonical
		} else {
			// The client wants to use a different digest algorithm. They'll just
			// have to be patient and wait for us to download and re-hash the
			// uploaded content using that digest algorithm.
			fullHash = true
		}
	} else if err == errResumableDigestNotAvailable {
		// Not using resumable digests, so we need to hash the entire layer.
		fullHash = true
	} else {
		return distribution.Descriptor{}, err
	}

	if fullHash {
		// a fantastic optimization: if the the written data and the size are
		// the same, we don't need to read the data from the backend. This is
		// because we've written the entire file in the lifecycle of the
		// current instance.
		if bw.written == size && digest.Canonical == desc.Digest.Algorithm() {
			canonical = bw.digester.Digest()
			verified = desc.Digest == canonical
		}

		// If the check based on size fails, we fall back to the slowest of
		// paths. We may be able to make the size-based check a stronger
		// guarantee, so this may be defensive.
		if !verified {
			digester := digest.Canonical.Digester()
			verifier := desc.Digest.Verifier()

			// Read the file from the backend driver and validate it.
			fr, err := newFileReader(ctx, bw.driver, bw.path, desc.Size)
			if err != nil {
				return distribution.Descriptor{}, err
			}
			defer fr.Close()

			tr := io.TeeReader(fr, digester.Hash())

			if _, err := io.Copy(verifier, tr); err != nil {
				return distribution.Descriptor{}, err
			}

			canonical = digester.Digest()
			verified = verifier.Verified()
		}
	}

	if !verified {
		context.GetLoggerWithFields(ctx,
			map[interface{}]interface{}{
				"canonical": canonical,
				"provided":  desc.Digest,
			}, "canonical", "provided").
			Errorf("canonical digest does match provided digest")
		return distribution.Descriptor{}, distribution.ErrBlobInvalidDigest{
			Digest: desc.Digest,
			Reason: fmt.Errorf("content does not match digest"),
		}
	}

	// update desc with canonical hash
	desc.Digest = canonical

	if desc.MediaType == "" {
		desc.MediaType = "application/octet-stream"
	}

	return desc, nil
}

// moveBlob moves the data into its final, hash-qualified destination,
// identified by dgst. The layer should be validated before commencing the
// move.
func (bw *blobWriter) moveBlob(ctx context.Context, desc distribution.Descriptor) error {
	blobPath, err := pathFor(blobDataPathSpec{
		digest: desc.Digest,
	})

	if err != nil {
		return err
	}

	// Check for existence
	if _, err := bw.blobStore.driver.Stat(ctx, blobPath); err != nil {
		switch err := err.(type) {
		case storagedriver.PathNotFoundError:
			break // ensure that it doesn't exist.
		default:
			return err
		}
	} else {
		// If the path exists, we can assume that the content has already
		// been uploaded, since the blob storage is content-addressable.
		// While it may be corrupted, detection of such corruption belongs
		// elsewhere.
		return nil
	}

	// If no data was received, we may not actually have a file on disk. Check
	// the size here and write a zero-length file to blobPath if this is the
	// case. For the most part, this should only ever happen with zero-length
	// blobs.
	if _, err := bw.blobStore.driver.Stat(ctx, bw.path); err != nil {
		switch err := err.(type) {
		case storagedriver.PathNotFoundError:
			// HACK(stevvooe): This is slightly dangerous: if we verify above,
			// get a hash, then the underlying file is deleted, we risk moving
			// a zero-length blob into a nonzero-length blob location. To
			// prevent this horrid thing, we employ the hack of only allowing
			// to this happen for the digest of an empty blob.
			if desc.Digest == digestSha256Empty {
				return bw.blobStore.driver.PutContent(ctx, blobPath, []byte{})
			}

			// We let this fail during the move below.
			logrus.
				WithField("upload.id", bw.ID()).
				WithField("digest", desc.Digest).Warnf("attempted to move zero-length content with non-zero digest")
		default:
			return err // unrelated error
		}
	}

	// TODO(stevvooe): We should also write the mediatype when executing this move.

	return bw.blobStore.driver.Move(ctx, bw.path, blobPath)
}

// removeResources should clean up all resources associated with the upload
// instance. An error will be returned if the clean up cannot proceed. If the
// resources are already not present, no error will be returned.
func (bw *blobWriter) removeResources(ctx context.Context) error {
	dataPath, err := pathFor(uploadDataPathSpec{
		name: bw.blobStore.repository.Named().Name(),
		id:   bw.id,
	})

	if err != nil {
		return err
	}

	// Resolve and delete the containing directory, which should include any
	// upload related files.
	dirPath := path.Dir(dataPath)
	if err := bw.blobStore.driver.Delete(ctx, dirPath); err != nil {
		switch err := err.(type) {
		case storagedriver.PathNotFoundError:
			break // already gone!
		default:
			// This should be uncommon enough such that returning an error
			// should be okay. At this point, the upload should be mostly
			// complete, but perhaps the backend became unaccessible.
			context.GetLogger(ctx).Errorf("unable to delete layer upload resources %q: %v", dirPath, err)
			return err
		}
	}

	return nil
}

func (bw *blobWriter) Reader() (io.ReadCloser, error) {
	// todo(richardscothern): Change to exponential backoff, i=0.5, e=2, n=4
	try := 1
	for try <= 5 {
		_, err := bw.driver.Stat(bw.ctx, bw.path)
		if err == nil {
			break
		}
		switch err.(type) {
		case storagedriver.PathNotFoundError:
			context.GetLogger(bw.ctx).Debugf("Nothing found on try %d, sleeping...", try)
			time.Sleep(1 * time.Second)
			try++
		default:
			return nil, err
		}
	}

	readCloser, err := bw.driver.Reader(bw.ctx, bw.path, 0)
	if err != nil {
		return nil, err
	}

	return readCloser, nil
}
