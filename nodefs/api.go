// Copyright 2019 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package nodefs provides infrastructure to build tree-organized filesystems.
//
// A tree-organized filesystem is similar to UNIX or Plan 9 filesystem: it
// consists of nodes with each node being either a file or a directory. Files
// are located at tree leafs. A directory node can have other nodes as its
// children and refer to each child by name unique through the directory.
// There can be several paths leading from tree root to a particular node,
// known as hard-linking, for example
//
//	    root
//	    /  \
//	  dir1 dir2
//	    \  /
//	    file
//
// A /-separated string path describes location of a node in the tree. For example
//
//	dir1/file
//
// describes path root → dir1 → file.
//
// Each node is associated with integer ID uniquely identifying the node
// throughout filesystem. The tree-level structure of any filesystem is
// expressed through index-nodes (also known as "inode", see Inode) which
// describe parent/child relation in between nodes and node-ID association.
//
// A particular filesystem should provide nodes with filesystem
// operations implemented as defined by Operations interface. When
// filesystem is mounted, its root Operations is associated with root
// of the tree, and the tree is further build lazily when nodefs
// infrastructure needs to lookup children of nodes to process client
// requests. For every new Operations, the filesystem infrastructure
// automatically builds new index node and links it in the filesystem
// tree. Operations.Inode() can be used to get particular Inode
// associated with a Operations.
//
// The kernel can evict inode data to free up memory. It does so by
// issuing FORGET calls. When a node has no children, and no kernel
// references, it is removed from the file system trees.
//
// File system trees can also be constructed in advance. This is done
// by instantiating "persistent" inodes from the Operations.OnAdd
// method. Persistent inodes remain in memory even if the kernel has
// forgotten them.  See zip_test.go for an example of how to do this.
//
// File systems whose tree structures are on backing storage typically
// discover the file system tree on-demand, and if the kernel is tight
// on memory, parts of the tree are forgotten again. These file
// systems should implement Operations.Lookup instead.  The loopback
// file system created by `NewLoopbackRoot` provides a straightforward
// example.
//
package nodefs

import (
	"context"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/fuse"
)

// InodeLink provides the machinery to connect Operations (user
// defined methods) to Inode (a node in the filesystem tree).
type InodeLink interface {
	// populateInode and inode are used by nodefs internally to
	// link Inode to a Node.
	//
	// See Inode() for the public API to retrieve an inode from Node.
	inode() *Inode
	init(ops Operations, attr NodeAttr, bridge *rawBridge, persistent bool)

	// Inode returns the *Inode associated with this Operations
	// instance.  The identity of the Inode does not change over
	// the lifetime of the node object.  Inode() is provided by
	// OperationStubs, and should not be reimplemented.
	Inode() *Inode
}

// Operations is the interface that implements the filesystem inode.
// Each Operations instance must embed OperationStubs. All error
// reporting must use the syscall.Errno type. The value 0 (`OK`)
// should be used to indicate success. The method names are inspired
// on the system call names, so we have Listxattr rather than
// ListXAttr.
type Operations interface {
	InodeLink

	// Statfs implements statistics for the filesystem that holds
	// this Inode.
	Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno

	// Access should return if the caller can access the file with
	// the given mode. In this case, the context has data about
	// the real UID. For example a root-SUID binary called by user
	// susan gets the UID and GID for susan here.
	Access(ctx context.Context, mask uint32) syscall.Errno

	// GetAttr reads attributes for an Inode. The library will
	// ensure that Mode and Ino are set correctly. For regular
	// files, Size should be set so it can be read correctly.
	Getattr(ctx context.Context, out *fuse.AttrOut) syscall.Errno

	// SetAttr sets attributes for an Inode.
	Setattr(ctx context.Context, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno

	// OnAdd is called once this Operations object is attached to
	// an Inode.
	OnAdd(ctx context.Context)
}

// XAttrOperations is a collection of methods used to implement extended attributes.
type XAttrOperations interface {
	Operations

	// GetXAttr should read data for the given attribute into
	// `dest` and return the number of bytes. If `dest` is too
	// small, it should return ERANGE and the size of the attribute.
	Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno)

	// SetXAttr should store data for the given attribute.  See
	// setxattr(2) for information about flags.
	Setxattr(ctx context.Context, attr string, data []byte, flags uint32) syscall.Errno

	// RemoveXAttr should delete the given attribute.
	Removexattr(ctx context.Context, attr string) syscall.Errno

	// ListXAttr should read all attributes (null terminated) into
	// `dest`. If the `dest` buffer is too small, it should return
	// ERANGE and the correct size.
	Listxattr(ctx context.Context, dest []byte) (uint32, syscall.Errno)
}

// SymlinkOperations holds operations specific to symlinks.
type SymlinkOperations interface {
	Operations

	// Readlink reads the content of a symlink.
	Readlink(ctx context.Context) ([]byte, syscall.Errno)
}

// FileOperations holds operations that apply to regular files.
type FileOperations interface {
	Operations

	// Open opens an Inode (of regular file type) for reading. It
	// is optional but recommended to return a FileHandle.
	Open(ctx context.Context, flags uint32) (fh FileHandle, fuseFlags uint32, errno syscall.Errno)

	// Reads data from a file. The data should be returned as
	// ReadResult, which may be constructed from the incoming
	// `dest` buffer. If the file was opened without FileHandle,
	// the FileHandle argument here is nil. The default
	// implementation forwards to the FileHandle.
	Read(ctx context.Context, f FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno)

	// Writes the data into the file handle at given offset. After
	// returning, the data will be reused and may not referenced.
	// The default implementation forwards to the FileHandle.
	Write(ctx context.Context, f FileHandle, data []byte, off int64) (written uint32, errno syscall.Errno)

	// Fsync is a signal to ensure writes to the Inode are flushed
	// to stable storage.  The default implementation forwards to the
	// FileHandle.
	Fsync(ctx context.Context, f FileHandle, flags uint32) syscall.Errno

	// Flush is called for close() call on a file descriptor. In
	// case of duplicated descriptor, it may be called more than
	// once for a file.   The default implementation forwards to the
	// FileHandle.
	Flush(ctx context.Context, f FileHandle) syscall.Errno

	// This is called to before the file handle is forgotten. The
	// kernel ingores the return value of this method,
	// so any cleanup that requires specific synchronization or
	// could fail with I/O errors should happen in Flush instead.
	// The default implementation forwards to the FileHandle.
	Release(ctx context.Context, f FileHandle) syscall.Errno

	// Allocate preallocates space for future writes, so they will
	// never encounter ESPACE.
	Allocate(ctx context.Context, f FileHandle, off uint64, size uint64, mode uint32) syscall.Errno

	// FGetattr is like Getattr but provides a file handle if available.
	Fgetattr(ctx context.Context, f FileHandle, out *fuse.AttrOut) syscall.Errno

	// FSetattr is like SetAttr but provides a file handle if available.
	Fsetattr(ctx context.Context, f FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno

	// CopyFileRange copies data between sections of two files,
	// without the data having to pass through the calling process.
	CopyFileRange(ctx context.Context, fhIn FileHandle,
		offIn uint64, out *Inode, fhOut FileHandle, offOut uint64,
		len uint64, flags uint64) (uint32, syscall.Errno)

	// Lseek is used to implement holes: it should return the
	// first offset beyond `off` where there is data (SEEK_DATA)
	// or where there is a hole (SEEK_HOLE).
	Lseek(ctx context.Context, f FileHandle, Off uint64, whence uint32) (uint64, syscall.Errno)
}

// LockOperations are operations for locking regions of regular files.
type LockOperations interface {
	FileOperations

	// Getlk returns locks that would conflict with the given
	// input lock. If no locks conflict, the output has type
	// L_UNLCK. See fcntl(2) for more information.
	Getlk(ctx context.Context, f FileHandle, owner uint64, lk *fuse.FileLock, flags uint32, out *fuse.FileLock) syscall.Errno

	// Setlk obtains a lock on a file, or fail if the lock could not
	// obtained.  See fcntl(2) for more information.
	Setlk(ctx context.Context, f FileHandle, owner uint64, lk *fuse.FileLock, flags uint32) syscall.Errno

	// Setlkw obtains a lock on a file, waiting if necessary. See fcntl(2)
	// for more information.
	Setlkw(ctx context.Context, f FileHandle, owner uint64, lk *fuse.FileLock, flags uint32) syscall.Errno
}

// DirStream lists directory entries.
type DirStream interface {
	// HasNext indicates if there are further entries. HasNext
	// might be called on already closed streams.
	HasNext() bool

	// Next retrieves the next entry. It is only called if HasNext
	// has previously returned true.  The Errno return may be used to
	// indicate I/O errors
	Next() (fuse.DirEntry, syscall.Errno)

	// Close releases resources related to this directory
	// stream.
	Close()
}

// DirOperations are operations for directory nodes in the filesystem.
type DirOperations interface {
	Operations

	// Lookup should find a direct child of the node by child
	// name.  If the entry does not exist, it should return ENOENT
	// and optionally set a NegativeTimeout in `out`. If it does
	// exist, it should return attribute data in `out` and return
	// the Inode for the child. A new inode can be created using
	// `Inode.NewInode`. The new Inode will be added to the FS
	// tree automatically if the return status is OK.
	Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*Inode, syscall.Errno)

	// OpenDir opens a directory Inode for reading its
	// contents. The actual reading is driven from ReadDir, so
	// this method is just for performing sanity/permission
	// checks.
	Opendir(ctx context.Context) syscall.Errno

	// ReadDir opens a stream of directory entries.
	Readdir(ctx context.Context) (DirStream, syscall.Errno)
}

// MutableDirOperations are operations for directories that can add or
// remove entries.
type MutableDirOperations interface {
	DirOperations

	// Mkdir is similar to Lookup, but must create a directory entry and Inode.
	Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*Inode, syscall.Errno)

	// Mknod is similar to Lookup, but must create a device entry and Inode.
	Mknod(ctx context.Context, name string, mode uint32, dev uint32, out *fuse.EntryOut) (*Inode, syscall.Errno)

	// Link is similar to Lookup, but must create a new link to an existing Inode.
	Link(ctx context.Context, target Operations, name string, out *fuse.EntryOut) (node *Inode, errno syscall.Errno)

	// Symlink is similar to Lookup, but must create a new symbolic link.
	Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (node *Inode, errno syscall.Errno)

	// Create is similar to Lookup, but should create a new
	// child. It typically also returns a FileHandle as a
	// reference for future reads/writes
	Create(ctx context.Context, name string, flags uint32, mode uint32) (node *Inode, fh FileHandle, fuseFlags uint32, errno syscall.Errno)

	// Unlink should remove a child from this directory.  If the
	// return status is OK, the Inode is removed as child in the
	// FS tree automatically.
	Unlink(ctx context.Context, name string) syscall.Errno

	// Rmdir is like Unlink but for directories.
	Rmdir(ctx context.Context, name string) syscall.Errno

	// Rename should move a child from one directory to a
	// different one. The changes is effected in the FS tree if
	// the return status is OK
	Rename(ctx context.Context, name string, newParent Operations, newName string, flags uint32) syscall.Errno
}

// FileHandle is a resource identifier for opened files.  FileHandles
// are useful in two cases: First, if the underlying storage systems
// needs a handle for reading/writing. See the function
// `NewLoopbackFile` for an example. Second, it is useful for
// implementing files whose contents are not tied to an inode. For
// example, a file like `/proc/interrupts` has no fixed content, but
// changes on each open call. This means that each file handle must
// have its own view of the content; this view can be tied to a
// FileHandle. Files that have such dynamic content should return the
// FOPEN_DIRECT_IO flag from their `Open` method. See directio_test.go
// for an example.
//
// For a description of individual operations, see the equivalent
// operations in FileOperations.
type FileHandle interface {
	Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno)

	Write(ctx context.Context, data []byte, off int64) (written uint32, errno syscall.Errno)

	Getlk(ctx context.Context, owner uint64, lk *fuse.FileLock, flags uint32, out *fuse.FileLock) syscall.Errno
	Setlk(ctx context.Context, owner uint64, lk *fuse.FileLock, flags uint32) syscall.Errno
	Setlkw(ctx context.Context, owner uint64, lk *fuse.FileLock, flags uint32) syscall.Errno

	Lseek(ctx context.Context, off uint64, whence uint32) (uint64, syscall.Errno)

	Flush(ctx context.Context) syscall.Errno

	Fsync(ctx context.Context, flags uint32) syscall.Errno

	Release(ctx context.Context) syscall.Errno

	Getattr(ctx context.Context, out *fuse.AttrOut) syscall.Errno
	Setattr(ctx context.Context, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno
	Allocate(ctx context.Context, off uint64, size uint64, mode uint32) syscall.Errno
}

// Options sets options for the entire filesystem
type Options struct {
	// MountOptions contain the options for mounting the fuse server
	fuse.MountOptions

	// If set to nonnil, this defines the overall entry timeout
	// for the file system. See fuse.EntryOut for more information.
	EntryTimeout *time.Duration

	// If set to nonnil, this defines the overall attribute
	// timeout for the file system. See fuse.EntryOut for more
	// information.
	AttrTimeout *time.Duration

	// If set to nonnil, this defines the overall entry timeout
	// for failed lookups (fuse.ENOENT). See fuse.EntryOut for
	// more information.
	NegativeTimeout *time.Duration

	// Automatic inode numbers are handed out sequentially
	// starting from this number. If unset, use 2^63.
	FirstAutomaticIno uint64
}
