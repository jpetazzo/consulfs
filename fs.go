// ConsulFS implements a FUSE filesystem that is backed by a Consul Key-Value store.
//
// API Usage
// ---------
// ConsulFS is implemented using the "bazil.org/fuse" package as a file system service.
// Refer to the fuse package for complete documentation on how to create a new FUSE
// connection that services a mount point. To have that mount point serve ConsulFS files,
// create a new instance of `ConsulFs`, give it to an "bazil.org/fuse/fs.Server", and call
// its Serve() method. The source code for the mounter at
// "github.com/bwester/consulfs/bin/consulfs" gives a full example of how to perform the
// mounting.
//
// The `ConsulFs` instance contains common configuration data and is referenced by all
// file and directory inodes. Notably, 'Uid' and 'Gid' set the uid and gid ownership of
// all files. The `Consul` option is used to perform all Consul HTTP RPCs. The
// `CancelConsulKv` struct is included in this package as a wrapper around the standard
// Consul APIs. It is vital for system stability that the file system not get into an
// uninteruptable sleep waiting for a remote RPC to complete, so CancelConsulKv will
// abandon requests when needed.
package consulfs

import (
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwester/consulfs/Godeps/_workspace/src/bazil.org/fuse"
	"github.com/bwester/consulfs/Godeps/_workspace/src/bazil.org/fuse/fs"
	"github.com/bwester/consulfs/Godeps/_workspace/src/bazil.org/fuse/fuseutil"
	"github.com/bwester/consulfs/Godeps/_workspace/src/github.com/Sirupsen/logrus"
	consul "github.com/bwester/consulfs/Godeps/_workspace/src/github.com/hashicorp/consul/api"
	"github.com/bwester/consulfs/Godeps/_workspace/src/golang.org/x/net/context"
)

// Unsupported features:
// * File modes other than 0600
// * Owners other than the mounter
// * Querying Consul ACLs to determine access permissions
// * Renaming directories
//
// Incomplete features:
// * Renaming files
// * Testing on Linux
// * Testing
// * Caching
//
// Known Bugs:
// * Opening a file in append mode and writing to it doesn't append. Instead, data
//   is overwritten at the beginning of the file. Unfortunately, the kernel doesn't expose
//   the O_APPEND flag to FUSE open requests. It probably does the appends by issuing
//   writes to the "end" of the file, where the end is defined by the size, which is
//   faked to be zero.
//
// With its current feature set, ConsulFs can be used for basic access with core POSIX tools.
// More complex uses, like compiling and linking an executable, break horribly in strange
// ways.

// The number of time a write will be attempted before returning an error
const MaxWriteAttempts = 10

// File is a single file's inode in the filesystem. It is backed by a key in Consul.
type File struct {
	ConsulFs *ConsulFs
	Key      string // The full keyname in Consul

	// Mutex guards all mutable metadata
	Mutex   sync.Mutex
	Ctime   time.Time // File attr
	Mtime   time.Time // File attr
	Atime   time.Time // File attr
	IsOpen  bool      // Is there an open handle to this file?
	Size    uint64    // If the file is open, the expected file size
	Deleted bool      // Whether this file has been deleted
	Buf     []byte    // If the file is deleted, buffers data locally
}

// SetDeleted marks this file as deleted.
//
// If the file is open, Posix says those processes should continue to operate on the file
// as if it exists, but when they close, it is removed. These semantics are preserved by
// caching a copy of the file and operating on that copy, letting the key on Consul be
// deleted eagerly.
func (file *File) SetDeleted(ctx context.Context) error {
	// If the file is already deleted, there is nothing more to do
	file.Mutex.Lock()
	if file.Deleted {
		file.Mutex.Unlock()
		file.ConsulFs.Logger.WithField("key", file.Key).Warning("SetDeleted() on deleted file")
		return fuse.ENOENT
	}
	if !file.IsOpen {
		file.Deleted = true
		file.Buf = make([]byte, 0) // just in case
		file.Mutex.Unlock()
		return nil
	}
	file.Mutex.Unlock()

	// Get a copy of the value to cache
	pair, _, err := file.ConsulFs.Consul.Get(ctx, file.Key, nil)

	file.Mutex.Lock()
	defer file.Mutex.Unlock()
	if file.Deleted {
		file.ConsulFs.Logger.WithField("key", file.Key).Warning("SetDeleted() file became deleted mid-call")
		return fuse.ENOENT
	}
	if err == ErrCanceled {
		return fuse.EINTR
	}
	if err != nil {
		file.ConsulFs.Logger.WithFields(logrus.Fields{
			"key":           file.Key,
			logrus.ErrorKey: err,
		}).Error("consul read error")
		return fuse.EIO
	}

	file.Deleted = true
	if pair == nil {
		// Key must have been deleted externally. That data's gone, no way to preserve
		// Posix semantics now. But the local entry still needs to be deleted so the
		// file doesn't get opened again.
		file.Buf = make([]byte, 0)
	} else {
		file.Buf = pair.Value
	}
	return nil
}

// lockedAttr fills in an Attr struct for this file. Call when the file's mutex is locked.
func (file *File) lockedAttr(attr *fuse.Attr) {
	attr.Mode = 0600
	if !file.Deleted {
		attr.Nlink = 1
	}
	// Timestamps aren't reflected in Consul, but it doesn't hurt to fake them
	attr.Ctime = file.Ctime
	attr.Mtime = file.Mtime
	attr.Atime = file.Atime
	attr.Uid = file.ConsulFs.Uid
	attr.Gid = file.ConsulFs.Gid
	if file.IsOpen {
		// Some applications like seeking...
		attr.Size = file.Size
	}
}

// Attr implements the Node interface. It is called when fetching the inode attribute for
// this file (e.g., to service stat(2)).
func (file *File) Attr(ctx context.Context, attr *fuse.Attr) error {
	file.Mutex.Lock()
	defer file.Mutex.Unlock()
	file.lockedAttr(attr)
	return nil
}

// BufferRead returns locally-buffered file contents, which will only be used if the file
// is deleted.
func (file *File) BufferRead() ([]byte, bool) {
	file.Mutex.Lock()
	defer file.Mutex.Unlock()
	if !file.Deleted {
		return nil, false
	}
	data := make([]byte, len(file.Buf))
	copy(data, file.Buf)
	return data, true
}

// Read implements the HandleReader interface. It is called to handle every read request.
// Because the file is opened in DirectIO mode, the kernel will not cache any file data.
func (file *File) Read(
	ctx context.Context,
	req *fuse.ReadRequest,
	resp *fuse.ReadResponse,
) error {
	data, err := file.ReadAll_(ctx)
	if err != nil {
		return err
	}
	fuseutil.HandleRead(req, resp, data)
	return nil
}

// ReadAll_ handles every read request by fetching the key from the server. This leads to
// simple consistency guarantees, as there is no caching, but performance may suffer in
// distributed settings. It intentionally does not implement the ReadAller interface to
// avoid the caching inherent in that interface.
func (file *File) ReadAll_(ctx context.Context) ([]byte, error) {
	// If the file has been removed from its directory, then data will come from the local
	// cache only.
	if data, ok := file.BufferRead(); ok {
		return data, nil
	}

	// Note that for complex caching semantics: the key has a 'CreateIndex' property that
	// could be used to distinguish a file's generation.

	// Query Consul for the full value for the file's key
	pair, _, err := file.ConsulFs.Consul.Get(ctx, file.Key, nil)
	if data, ok := file.BufferRead(); ok {
		return data, nil
	}
	if err == ErrCanceled {
		return nil, fuse.EINTR
	} else if err != nil {
		file.ConsulFs.Logger.WithFields(logrus.Fields{
			"key":           file.Key,
			logrus.ErrorKey: err,
		}).Error("consul read error")
		return nil, fuse.EIO
	}
	if pair == nil {
		return nil, fuse.ENOENT
	}
	return pair.Value, nil
}

// doWrite does the buffer manipulation to perform a write. Data buffers are kept
// contiguous.
func doWrite(
	offset int64,
	writeData []byte,
	fileData []byte,
) []byte {
	fileEnd := int64(len(fileData))
	writeEnd := offset + int64(len(writeData))
	var buf []byte
	if writeEnd > fileEnd {
		buf = make([]byte, writeEnd)
		if fileEnd <= offset {
			copy(buf, fileData)
		} else {
			copy(buf, fileData[:offset])
		}
	} else {
		buf = fileData
	}
	copy(buf[offset:writeEnd], writeData)
	return buf
}

func (file *File) BufferWrite(req *fuse.WriteRequest, resp *fuse.WriteResponse) bool {
	file.Mutex.Lock()
	defer file.Mutex.Unlock()
	if !file.Deleted {
		return false
	}
	file.Buf = doWrite(req.Offset, req.Data, file.Buf)
	resp.Size = len(req.Data)
	return true
}

// Write implements the HandleWriter interface. It is called on *every* write (DirectIO
// mode) to allow this module to handle consistency itself. Current strategy is to read
// the file, change the written portions, then write it back atomically. If the key was
// updated between the read and the write, try again.
func (file *File) Write(
	ctx context.Context,
	req *fuse.WriteRequest,
	resp *fuse.WriteResponse,
) error {
	for attempts := 0; attempts < MaxWriteAttempts; attempts++ {
		if file.BufferWrite(req, resp) {
			return nil
		}
		pair, _, err := file.ConsulFs.Consul.Get(ctx, file.Key, nil)
		if file.BufferWrite(req, resp) {
			return nil
		}
		if err == ErrCanceled {
			return fuse.EINTR
		} else if err != nil {
			file.ConsulFs.Logger.WithFields(logrus.Fields{
				"key":           file.Key,
				logrus.ErrorKey: err,
			}).Error("consul read error")
			return fuse.EIO
		}
		if pair == nil {
			return fuse.ENOENT
		}

		pair.Value = doWrite(req.Offset, req.Data, pair.Value)

		// Write it back!
		success, _, err := file.ConsulFs.Consul.CAS(ctx, pair, nil)
		if file.BufferWrite(req, resp) {
			return nil
		}
		if err == ErrCanceled {
			return fuse.EINTR
		} else if err != nil {
			file.ConsulFs.Logger.WithFields(logrus.Fields{
				"key":           file.Key,
				logrus.ErrorKey: err,
			}).Error("consul write error")
			return fuse.EIO
		}
		if success {
			resp.Size = len(req.Data)
			return nil
		}
		file.ConsulFs.Logger.WithField("key", file.Key).Warning("write did not succeed")
	}
	file.ConsulFs.Logger.WithField("key", file.Key).Error("unable to perform timely write; aborting")
	return fuse.EIO
}

// Fsync implements the NodeFsyncer interface. It is called to explicitly flush cached
// data to storage (e.g., on a fsync(2) call). Since data is not cached, this is a no-op.
func (file *File) Fsync(
	ctx context.Context,
	req *fuse.FsyncRequest,
) error {
	return nil
}

// doTruncate implements the buffer manipulation needed to truncate a file's contents.
func doTruncate(buf []byte, size uint64) ([]byte, bool) {
	bufLen := uint64(len(buf))
	if bufLen == size {
		return buf, false
	}
	if bufLen > size {
		return buf[:size], true
	}
	if uint64(cap(buf)) >= size {
		buf = buf[:size]
		for i := bufLen; i < size; i++ {
			buf[i] = 0
		}
		return buf, true
	}
	newBuf := make([]byte, size)
	copy(newBuf, buf)
	return newBuf, true
}

func (file *File) BufferTruncate(size uint64) bool {
	file.Mutex.Lock()
	defer file.Mutex.Unlock()
	if !file.Deleted {
		return false
	}
	file.Buf, _ = doTruncate(file.Buf, size)
	return true
}

// Truncate sets a key's value to the given size, stripping data off the end or adding \0
// as needed. Note that a Consul Key-Value pair has two data segments, "value" and
// "flags," and this operation only changes the value. So to preserve the flags, a full
// read-modify-write must be done, even when the value is cleared entirely.
func (file *File) Truncate(
	ctx context.Context,
	size uint64,
) error {
	for attempts := 0; attempts < MaxWriteAttempts; attempts++ {
		if file.BufferTruncate(size) {
			return nil
		}
		// Read the contents of the key
		pair, _, err := file.ConsulFs.Consul.Get(ctx, file.Key, nil)
		if file.BufferTruncate(size) {
			return nil
		}
		if err == ErrCanceled {
			return fuse.EINTR
		} else if err != nil {
			file.ConsulFs.Logger.WithFields(logrus.Fields{
				"key":           file.Key,
				logrus.ErrorKey: err,
			}).Error("consul read error")
			return fuse.EIO
		}
		if pair == nil {
			return fuse.ENOENT
		}

		var changed bool
		pair.Value, changed = doTruncate(pair.Value, size)
		if !changed {
			return nil
		}

		// Write the results back
		success, _, err := file.ConsulFs.Consul.CAS(ctx, pair, nil)
		if file.BufferTruncate(size) {
			return nil
		}
		if err == ErrCanceled {
			return fuse.EINTR
		} else if err != nil {
			file.ConsulFs.Logger.WithFields(logrus.Fields{
				"key":           file.Key,
				logrus.ErrorKey: err,
			}).Error("consul write error")
			return fuse.EIO
		}
		if success {
			return nil
		}
		file.ConsulFs.Logger.WithField("key", file.Key).Warning("truncate did not succeed")
	}
	return fuse.EINTR
}

// Setattr implements the fs.NodeSetattrer interface. This is used by the kernel to
// request metadata changes, including the file's size (used by ftruncate(2) or by
// open("...", O_TRUNC) to clear a file's content).
func (file *File) Setattr(
	ctx context.Context,
	req *fuse.SetattrRequest,
	resp *fuse.SetattrResponse,
) error {
	if req.Valid.Uid() || req.Valid.Gid() {
		return fuse.ENOTSUP
	}
	// Support only idempotent writes. This is needed so cp(1) can copy a file.
	if req.Valid.Mode() && req.Mode != 0600 {
		return fuse.EPERM
	}
	// The truncate operation could fail, so do it first before altering any other file
	// metadata in an attempt to keep this setattr request atomic.
	if req.Valid.Size() {
		err := file.Truncate(ctx, req.Size)
		if err != nil {
			return err
		}
	}
	file.Mutex.Lock()
	defer file.Mutex.Unlock()
	now := time.Now()
	if req.Valid.Atime() {
		file.Atime = req.Atime
	}
	if req.Valid.AtimeNow() {
		file.Atime = now
	}
	if req.Valid.Mtime() {
		file.Mtime = req.Mtime
	}
	if req.Valid.MtimeNow() {
		file.Mtime = now
	}
	file.lockedAttr(&resp.Attr)
	return nil
}

// Open implements the NodeOpener interface. It is called the first time a file is opened
// by any process. Further opens or FD duplications will reuse this handle. When all FDs
// have been closed, Release() will be called.
func (file *File) Open(
	ctx context.Context,
	req *fuse.OpenRequest,
	resp *fuse.OpenResponse,
) (fs.Handle, error) {
	// Using the DirectIO flag disables the kernel buffer cache. *Every* read and write
	// will be passed directly to this module. This gives the module greater control over
	// the file's consistency model.
	resp.Flags |= fuse.OpenDirectIO
	file.Mutex.Lock()
	defer file.Mutex.Unlock()
	file.IsOpen = true
	return file, nil
}

// Release implements the HandleReleaser interface. It is called when all file descriptors
// to the file have been closed.
func (file *File) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	file.Mutex.Lock()
	defer file.Mutex.Unlock()
	file.IsOpen = false
	return nil
}

// Dir represents a directory inode in VFS. Directories don't actually exist in Consul.
// TODO: discuss the strategy used to fake dirs.
type Dir struct {
	ConsulFs *ConsulFs
	Prefix   string
	Level    uint

	// The mutex protects cached directory contents
	mux       sync.Mutex
	expires   time.Time
	readIndex uint64
	files     map[string]*File
	dirs      map[string]*Dir
}

func (dir *Dir) NewFile(key string) *File {
	now := time.Now()
	return &File{
		ConsulFs: dir.ConsulFs,
		Key:      key,
		Ctime:    now,
		Mtime:    now,
		Atime:    now,
	}
}

func (dir *Dir) NewDir(prefix string) *Dir {
	return &Dir{
		ConsulFs: dir.ConsulFs,
		Prefix:   prefix,
		Level:    dir.Level + 1,
		files:    make(map[string]*File),
		dirs:     make(map[string]*Dir),
	}
}

// Attr implements the Node interface. It is called when fetching the inode attribute for
// this directory (e.g., to service stat(2)).
func (dir *Dir) Attr(ctx context.Context, attr *fuse.Attr) error {
	attr.Mode = os.ModeDir | 0700
	// Nlink should technically include all the files in the directory, but VFS seems fine
	// with the constant "2".
	attr.Nlink = 2
	attr.Uid = dir.ConsulFs.Uid
	attr.Gid = dir.ConsulFs.Gid
	return nil
}

// Lookup implements the NodeStringLookuper interface, to look up a directory entry by
// name. This is called to get the inode for the given name. The name doesn't have to have
// been returned by ReadDirAll() for a process to attempt to find it!
func (dir *Dir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	err := dir.Refresh(ctx)
	if err != nil {
		return nil, err
	}
	dir.mux.Lock()
	defer dir.mux.Unlock()
	// Search directories first. If there is a key that ends in a "/", allow it
	// to be masked.
	if d, ok := dir.dirs[name]; ok {
		return d, nil
	} else if f, ok := dir.files[name]; ok {
		return f, nil
	}
	return nil, fuse.ENOENT
}

func (dir *Dir) Refresh(ctx context.Context) error {
	// Check if the cached directory listing has expired
	now := time.Now()
	dir.mux.Lock()
	expires := dir.expires
	dir.mux.Unlock()
	if expires.After(now) {
		return nil
	}

	// Call Consul to get an updated listing. This could block for a while, so
	// do not hold the dir lock while calling.
	keys, meta, err := dir.ConsulFs.Consul.Keys(ctx, dir.Prefix, "/", nil)
	if err == ErrCanceled {
		return fuse.EINTR
	} else if err != nil {
		dir.ConsulFs.Logger.WithFields(logrus.Fields{
			"prefix":        dir.Prefix,
			logrus.ErrorKey: err,
		}).Error("consul list error")
		return fuse.EIO
	}
	// Reminder: if the directory is empty, `keys` could be `nil`.

	// If another, later update completed while waiting on Consul, ignore
	// these results.
	dir.mux.Lock()
	defer dir.mux.Unlock()
	if dir.readIndex > meta.LastIndex {
		return nil
	}
	dir.readIndex = meta.LastIndex
	dir.expires = time.Now().Add(1 * time.Second)

	// Add new files and directories
	prefixLen := len(dir.Prefix)
	fileNames := map[string]bool{}
	for _, key := range keys {
		if !strings.HasPrefix(key, dir.Prefix) {
			dir.ConsulFs.Logger.WithFields(logrus.Fields{
				"prefix": dir.Prefix,
				"key":    key,
			}).Warning("list included invalid key")
			continue
		}
		if key == dir.Prefix {
			// Pathological case: a key's full name ended in "/", making it look like
			// a directory, and now the OS is trying to list that directory.
			continue
		}
		name := key[prefixLen:]
		if strings.HasSuffix(name, "/") {
			// Probably a directory
			dirName := name[:len(name)-1]
			if _, ok := dir.dirs[dirName]; !ok {
				dir.dirs[dirName] = dir.NewDir(key)
			}
		} else {
			// Data-holding key
			if _, ok := dir.files[name]; !ok {
				dir.files[name] = dir.NewFile(key)
			}
			fileNames[name] = true
		}
	}

	// Remove any files that are not present anymore
	for name := range dir.files {
		if !fileNames[name] {
			delete(dir.files, name)
		}
	}

	return nil
}

// ReadDirAll returns the entire contents of the directory when the directory is being
// listed (e.g., with "ls").
func (dir *Dir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	// Get Consul to refresh the local cache of the directory's entries
	err := dir.Refresh(ctx)
	if err != nil {
		return nil, err
	}
	dir.mux.Lock()
	defer dir.mux.Unlock()

	ents := make([]fuse.Dirent, 2, len(dir.files)+len(dir.dirs)+2)
	ents[0] = fuse.Dirent{Name: ".", Type: fuse.DT_Dir}
	ents[1] = fuse.Dirent{Name: "..", Type: fuse.DT_Dir}
	for fileName := range dir.files {
		ents = append(ents, fuse.Dirent{
			Name: fileName,
			Type: fuse.DT_File,
		})
	}
	for dirName := range dir.dirs {
		ents = append(ents, fuse.Dirent{
			Name: dirName,
			Type: fuse.DT_Dir,
		})
	}
	return ents, nil
}

// Create implements the NodeCreater interface. It is called to create and open a new
// file. The kernel will first try to Lookup the name, and this method will only be called
// if the name didn't exist.
func (dir *Dir) Create(
	ctx context.Context,
	req *fuse.CreateRequest,
	resp *fuse.CreateResponse,
) (fs.Node, fs.Handle, error) {
	// The filename can't contain the path separator. That would mess up how "directories"
	// are listed.
	if strings.Contains(req.Name, "/") {
		return nil, nil, fuse.EPERM
	}

	// Create the key first, then insert it into the directory structure
	key := dir.Prefix + req.Name
	pair := &consul.KVPair{
		Key:         key,
		ModifyIndex: 0, // Write will fail if the key already exists
		Flags:       0,
		Value:       []byte{},
	}
	success, _, err := dir.ConsulFs.Consul.CAS(ctx, pair, nil)
	if err == ErrCanceled {
		return nil, nil, fuse.EINTR
	} else if err != nil {
		dir.ConsulFs.Logger.WithFields(logrus.Fields{
			"key":           key,
			logrus.ErrorKey: err,
		}).Error("consul create error")
		return nil, nil, fuse.EIO
	}

	dir.mux.Lock()
	defer dir.mux.Unlock()
	// Success or failure, once the Consul CAS operation completes without an error, the key
	// exists in the store. Make sure it's in the local cache.
	var file *File
	var ok bool
	if file, ok = dir.files[req.Name]; !ok {
		file = dir.NewFile(key)
		dir.files[req.Name] = file
	}
	if !success {
		file.ConsulFs.Logger.WithField("key", key).Warning("create failed")
		return nil, nil, fuse.EEXIST
	}
	// Just like in File.Open()
	resp.OpenResponse.Flags |= fuse.OpenDirectIO
	return file, file, nil
}

// RemoveDir is called to remove a directory
func (dir *Dir) RemoveDir(ctx context.Context, req *fuse.RemoveRequest) error {
	// Look in the cache to find the child directory being removed.
	dir.mux.Lock()
	childDir, ok := dir.dirs[req.Name]
	if !ok {
		dir.mux.Unlock()
		return fuse.ENOENT
	}
	dir.mux.Unlock()

	// Don't delete a directory with files in it. To do that, we have to refresh the
	// file listing of the directory.
	err := childDir.Refresh(ctx)
	if err != nil {
		return err
	}
	dir.mux.Lock()
	defer dir.mux.Unlock()
	childDir.mux.Lock()
	defer childDir.mux.Unlock()

	// State could have changed while the refresh was happening
	childDir2, ok := dir.dirs[req.Name]
	if !ok {
		return fuse.ENOENT
	}
	if childDir != childDir2 {
		// Many concurrent removes? Just give up.
		return fuse.EIO
	}

	// Only delete an empty directory
	if len(childDir.dirs) > 0 || len(childDir.files) > 0 {
		return fuse.Errno(syscall.ENOTEMPTY)
	}
	delete(dir.dirs, req.Name)
	return nil
}

// RemoveFile is called to unlink a file.
func (dir *Dir) RemoveFile(ctx context.Context, req *fuse.RemoveRequest) error {
	// Get a reference to the file
	dir.mux.Lock()
	file, ok := dir.files[req.Name]
	dir.mux.Unlock()
	if !ok {
		// Something else removed it first?
		return nil
	}

	// Mark the file as deleted. If the file is open, this may make a blocking
	// call to Consul to cache the file's contents. If this call fails, the entire
	// Remove operation can be aborted without changing any state.
	err := file.SetDeleted(ctx)
	if err != nil {
		return err
	}

	// Once the file is marked as deleted, remove its entry so it can't be found
	// locally.
	dir.mux.Lock()
	file2, ok := dir.files[req.Name]
	if file != file2 || !ok {
		// Something else has already removed and replaced the file?!
		dir.mux.Unlock()
		return nil
	}
	delete(dir.files, req.Name)
	dir.mux.Unlock()

	// Finally, remove the file's key from Consul. Errors at this step are horrible.
	// Any process that had the file open already will be working on its own forked
	// copy, but the key will still exist on the server.
	_, err = dir.ConsulFs.Consul.Delete(ctx, file.Key, nil)
	if err == ErrCanceled {
		dir.ConsulFs.Logger.WithField("key", file.Key).Error("delete interrupted at a bad time")
		return fuse.EINTR
	} else if err != nil {
		dir.ConsulFs.Logger.WithFields(logrus.Fields{
			"key":           file.Key,
			logrus.ErrorKey: err,
		}).Error("consul delete error")
		return fuse.EIO
	}
	return nil
}

// Remove implements the NodeRemover interface. It is called to remove files or directory
// from a directory's contents.
func (dir *Dir) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	if req.Dir {
		return dir.RemoveDir(ctx, req)
	}
	return dir.RemoveFile(ctx, req)
}

// Mkdir implements the NodeMkdirer interface. It is called to make a new directory.
func (dir *Dir) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fs.Node, error) {
	// Since directories don't exist in Consul, this is easy! Make sure the name doesn't
	// already exist, then add an entry for it.
	err := dir.Refresh(ctx)
	if err != nil {
		return nil, err
	}
	dir.mux.Lock()
	defer dir.mux.Unlock()

	if _, ok := dir.dirs[req.Name]; ok {
		return nil, fuse.EEXIST
	}
	if _, ok := dir.files[req.Name]; ok {
		return nil, fuse.EEXIST
	}
	newDir := dir.NewDir(dir.Prefix + req.Name + "/")
	dir.dirs[req.Name] = newDir
	return newDir, nil
}

// lockSet holds a set of locked Dirs so they can be easily unlocked.
type lockSet []*Dir

// Len implements the sort.Interface interface. Returns the number of Dirs in the set.
func (ls lockSet) Len() int {
	return len([]*Dir(ls))
}

// Less implements the sort.Interface interface. Equivalent to: ls[i] < ls[j].
func (ls lockSet) Less(i, j int) bool {
	return ls[i].Level < ls[j].Level ||
		(ls[i].Level == ls[j].Level && ls[i].Prefix < ls[j].Prefix)
}

// Swap implements the sort.Interface interface. Swaps two elements.
func (ls lockSet) Swap(i, j int) {
	ls[i], ls[j] = ls[j], ls[i]
}

// Unlock will release the mutexes on every Dir in a lockSet.
func (ls lockSet) Unlock() {
	var last *Dir
	for _, d := range ls {
		if d != last {
			d.mux.Unlock()
		}
		last = d
	}
}

// lockDirs acquires the locks on the given directores in the canonical order to prevent
// deadlocks. Dirs are ordered by level in the tree, then by prefix. This lock
// order allows a parent directory to always be able to lock one of its children without
// needing to drop its own lock first.
func lockDirs(dirs ...*Dir) lockSet {
	ls := lockSet(dirs)
	sort.Sort(ls)
	var last *Dir
	for _, d := range ls {
		if d != last {
			d.mux.Lock()
		}
		last = d
	}
	return ls
}

// Rename implements the NodeRenamer interface. It's called to rename a file from one name
// to another, possibly in another directory. There is no plan to support renaming
// directories at this time. Consul doesn't have a rename operation, so the new name is
// written and the old one deleted as two separate actions. If the new name already exists
// as a file, it is replaced atomically.
func (dir *Dir) Rename(
	ctx context.Context,
	req *fuse.RenameRequest,
	newDirNode fs.Node,
) error {
	newDir, ok := newDirNode.(*Dir)
	if !ok {
		return fuse.ENOTSUP
	}
	var ndRefresh chan error
	if newDir != dir {
		ndRefresh = make(chan error)
		go func() { ndRefresh <- dir.Refresh(ctx) }()
	}
	err := dir.Refresh(ctx)
	if err != nil {
		return err
	}
	if ndRefresh != nil {
		err = <-ndRefresh
		if err != nil {
			return err
		}
	}

	// TODO: finish me

	return fuse.ENOTSUP
}

// ConsulFs is the main file system object that represents a Consul Key-Value store.
type ConsulFs struct {
	// Consul contains a referene to the ConsulCanceler that should be used for all operations.
	Consul ConsulCanceler

	// Uid contains the UID that will own all the files in the file system.
	Uid uint32

	// Gid contains the GID that will own all the files in the file system.
	Gid uint32

	// Messages will be sent to this logger
	Logger *logrus.Logger
}

// Root implements the fs.FS interface. It is called once to get the root directory inode
// for the mount point.
func (f *ConsulFs) Root() (fs.Node, error) {
	return &Dir{
		ConsulFs: f,
		Prefix:   "",
		Level:    0,
		files:    make(map[string]*File),
		dirs:     make(map[string]*Dir),
	}, nil
}
