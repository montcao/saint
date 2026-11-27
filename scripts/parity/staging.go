package main

import (
	"bytes"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
)

const stagedCommandLimit = 120

type stagedFile struct {
	name string
	info os.FileInfo
	data []byte
}
type scriptDirectory struct {
	path  string
	root  *scriptRoot
	info  os.FileInfo
	files []*stagedFile
}
type stagedScripts struct {
	hostPane, guestPane string
	host, guest         *scriptDirectory
	uncertain           bool
}

// scriptRoot holds the directory opened without following a symlink. All
// file operations are relative to this handle and use generated basenames.
type scriptRoot struct{ file *os.File }

func (r *scriptRoot) OpenFile(name string, flags int, mode os.FileMode) (*os.File, error) {
	if filepath.Base(name) != name || name == "." || name == ".." {
		return nil, errors.New("invalid script basename")
	}
	fd, err := unix.Openat(int(r.file.Fd()), name, flags|unix.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}
func (r *scriptRoot) Remove(name string) error { return unix.Unlinkat(int(r.file.Fd()), name, 0) }
func (r *scriptRoot) Close() error             { return r.file.Close() }
func newScriptDirectory(parent string) (*scriptDirectory, error) {
	return allocateScriptDirectory(parent, nil)
}
func allocateScriptDirectory(parent string, afterCreate func(*os.Root, string)) (*scriptDirectory, error) {
	parentRoot, err := os.OpenRoot(parent)
	if err != nil {
		return nil, errors.New("cannot pin diagnostic parent directory")
	}
	defer parentRoot.Close()
	name := ".sp-" + nonce()[:12]
	if err := parentRoot.Mkdir(name, 0700); err != nil {
		return nil, errors.New("cannot exclusively create diagnostic directory")
	}
	if afterCreate != nil {
		afterCreate(parentRoot, name)
	}
	// A shared parent may replace its entry. Never follow the replacement and
	// never delete an uncertain entry after an unsuccessful open.
	file, err := parentRoot.OpenFile(name, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("diagnostic directory open uncertain; entry retained")
	}
	info, err := file.Stat()
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		file.Close()
		return nil, errors.New("diagnostic directory identity uncertain; entry retained")
	}
	return &scriptDirectory{path: filepath.Join(parent, name), root: &scriptRoot{file: file}, info: info}, nil
}

// The host copy must never be executed from a guest-writable share. /tmp is
// excluded by Saint's repository guards; the operator must also ensure it is
// absent from any nonstandard guest mounts. Guest files are diagnostic inputs,
// not an execution integrity boundary against a hostile guest.
func newStagedScripts(repo, host, guest string) (*stagedScripts, error) {
	if !filepath.IsAbs(repo) || strings.IndexFunc(repo, unicode.IsControl) >= 0 {
		return nil, errors.New("stage-repo must be an absolute path without control characters")
	}
	canonical, err := filepath.EvalSymlinks(repo)
	if err != nil || canonical != filepath.Clean(repo) {
		return nil, errors.New("stage-repo must be an existing canonical directory")
	}
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return nil, err
	}
	// Use the conventional short temporary path, not a potentially long TMPDIR.
	shortTmp, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		return nil, err
	}
	for _, protected := range []string{tmp, shortTmp} {
		overlap, err := directoriesOverlap(canonical, protected)
		if err != nil || overlap {
			return nil, errors.New("stage-repo overlaps host-only temporary storage or cannot be inspected")
		}
	}

	h, err := newScriptDirectory(shortTmp)
	if err != nil {
		return nil, err
	}
	g, err := newScriptDirectory(canonical)
	if err != nil {
		_ = h.cleanup()
		return nil, err
	}
	return &stagedScripts{hostPane: host, guestPane: guest, host: h, guest: g}, nil
}

func (s *stagedScripts) prepare(pane, script, id string) (string, func() error, error) {
	dir := s.host
	if pane == s.guestPane {
		dir = s.guest
	} else if pane != s.hostPane {
		return "", nil, errors.New("pane does not own a staged-script directory")
	}
	name := fmt.Sprintf("p%d", len(dir.files))
	command := "sh " + quote(filepath.Join(dir.path, name))
	if len(command) > stagedCommandLimit || strings.IndexFunc(command, unicode.IsControl) >= 0 {
		return "", nil, errors.New("staged invocation exceeds the diagnostic input budget or contains controls; choose a shorter canonical repository path")
	}
	data := []byte(probeCommand("", script, id) + "\n")
	if len(data) > 65536 {
		return "", nil, errors.New("diagnostic script exceeds size limit")
	}
	f, err := dir.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", nil, errors.New("cannot exclusively create diagnostic script")
	}
	_, writeErr := f.Write(data)
	info, statErr := f.Stat()
	closeErr := f.Close()
	if err = errors.Join(writeErr, statErr, closeErr); err != nil {
		s.uncertain = true
		return "", nil, errors.New("diagnostic script write failed; artifacts retained")
	}
	record := &stagedFile{name: name, info: info, data: data}
	dir.files = append(dir.files, record)
	check := func() error { return dir.verify(record) }
	return command, check, check()
}

func (d *scriptDirectory) verify(record *stagedFile) error {
	current, err := os.Lstat(d.path)
	if err != nil || !os.SameFile(current, d.info) || current.Mode().Perm() != 0700 {
		return errors.New("diagnostic directory identity or permissions changed")
	}
	f, err := d.root.OpenFile(record.name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errors.New("diagnostic script cannot be inspected")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !os.SameFile(info, record.info) {
		return errors.New("diagnostic script identity or permissions changed")
	}
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || !bytes.Equal(data, record.data) {
		return errors.New("diagnostic script content changed")
	}
	return nil
}
func (d *scriptDirectory) cleanup() error {
	defer d.root.Close()
	for _, f := range d.files {
		if err := d.verify(f); err != nil {
			return err
		}
		if err := d.root.Remove(f.name); err != nil {
			return errors.New("owned diagnostic script cleanup failed")
		}
	}
	info, err := os.Lstat(d.path)
	if err != nil || !os.SameFile(info, d.info) {
		return errors.New("diagnostic directory changed; retained")
	}
	// Remove only an empty directory, never recursively remove unexpected files.
	if err := os.Remove(d.path); err != nil {
		return errors.New("diagnostic directory not empty or cleanup failed; retained")
	}
	return nil
}
func (s *stagedScripts) cleanup() error {
	if s.uncertain {
		s.host.root.Close()
		s.guest.root.Close()
		return errors.New("execution uncertain; diagnostic scripts retained for inspection")
	}
	// The guest can race directory-entry replacements while running. Never
	// unlink a shared-tree name based on an earlier identity check.
	return errors.Join(s.host.cleanup(), s.guest.root.Close())
}

// Compare actual directory identities in both ancestor chains, including
// differently capitalized aliases on a case-insensitive filesystem.
func directoriesOverlap(a, b string) (bool, error) {
	ancestor := func(child, parent string) (bool, error) {
		target, err := os.Stat(parent)
		if err != nil {
			return false, err
		}
		if !target.IsDir() {
			return false, errors.New("not a directory")
		}
		for {
			info, err := os.Stat(child)
			if err != nil {
				return false, err
			}
			if !info.IsDir() {
				return false, errors.New("not a directory")
			}
			if os.SameFile(info, target) {
				return true, nil
			}
			next := filepath.Dir(child)
			if next == child {
				return false, nil
			}
			child = next
		}
	}
	if yes, err := ancestor(a, b); yes || err != nil {
		return yes, err
	}
	return ancestor(b, a)
}
