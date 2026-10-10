package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/himanshubaliyan7/warmbox/internal/agentapi"
)

// The file routes operate on the sandbox's own filesystem with the
// agent's own (unprivileged) identity. There is no path jail: the agent
// is inside the sandbox and anything it can reach, sandboxed code can
// reach too. Relative paths resolve against the workspace.

func (a *Agent) filePath(r *http.Request) (string, error) {
	p := r.URL.Query().Get("path")
	if p == "" {
		return "", badRequest("path is required")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(a.cfg.Workspace, p)
	}
	return filepath.Clean(p), nil
}

// fileError maps a filesystem error to an API error:
// missing -> 404, permission or read-only -> 403, wrong kind of object,
// already exists or not empty -> 409, no space -> 507.
//
// The 409 "conflict" cases are: ENOTDIR (a component of the path, or the
// directory to list, is a file), EEXIST (mkdir over a file; a write whose
// parent directory cannot be created because a file is in the way),
// ENOTEMPTY and EISDIR (a write renamed onto a directory that appeared
// meanwhile). A read or write whose target is a directory is the more
// specific 409 "is_directory".
func fileError(err error) error {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &apiError{http.StatusNotFound, "not_found", err.Error()}
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EROFS):
		return &apiError{http.StatusForbidden, "permission_denied", err.Error()}
	case errors.Is(err, fs.ErrExist), errors.Is(err, syscall.ENOTEMPTY),
		errors.Is(err, syscall.EISDIR), errors.Is(err, syscall.ENOTDIR):
		return &apiError{http.StatusConflict, "conflict", err.Error()}
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return &apiError{http.StatusInsufficientStorage, "no_space", err.Error()}
	}
	return &apiError{http.StatusInternalServerError, "io_error", err.Error()}
}

func (a *Agent) handleFileRead(w http.ResponseWriter, r *http.Request) {
	path, err := a.filePath(r)
	if err != nil {
		writeError(w, err)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeError(w, fileError(err))
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeError(w, fileError(err))
		return
	}
	if fi.IsDir() {
		writeError(w, &apiError{http.StatusConflict, "is_directory", path + " is a directory"})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if fi.Mode().IsRegular() {
		w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	}
	w.WriteHeader(http.StatusOK)
	// A failure from here on can only be signalled by cutting the body
	// short, which the client detects through Content-Length.
	_, _ = io.Copy(w, f)
}

// handleFileWrite streams the body to a temporary file next to the
// target and renames it into place, so readers never see a partial file.
func (a *Agent) handleFileWrite(w http.ResponseWriter, r *http.Request) {
	path, err := a.filePath(r)
	if err != nil {
		writeError(w, err)
		return
	}
	mode := os.FileMode(0o644)
	if s := r.URL.Query().Get("mode"); s != "" {
		n, err := strconv.ParseUint(s, 8, 32)
		if err != nil || n > 0o7777 {
			writeError(w, badRequest("mode must be octal permission bits, e.g. 0644"))
			return
		}
		mode = os.FileMode(n & 0o777)
		if n&0o4000 != 0 {
			mode |= os.ModeSetuid
		}
		if n&0o2000 != 0 {
			mode |= os.ModeSetgid
		}
		if n&0o1000 != 0 {
			mode |= os.ModeSticky
		}
	}
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		writeError(w, &apiError{http.StatusConflict, "is_directory", path + " is a directory"})
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, fileError(err))
		return
	}
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".sbx-"+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		writeError(w, fileError(err))
		return
	}
	fail := func(err error) {
		_ = f.Close()
		_ = os.Remove(tmp)
		writeError(w, err)
	}
	if _, err := io.Copy(f, r.Body); err != nil {
		// Either the upload broke or the write failed; tell them apart.
		var pe *fs.PathError
		if errors.As(err, &pe) || errors.Is(err, syscall.ENOSPC) {
			fail(fileError(err))
		} else {
			fail(badRequest("reading body: %v", err))
		}
		return
	}
	if err := f.Chmod(mode); err != nil {
		fail(fileError(err))
		return
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		writeError(w, fileError(err))
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		writeError(w, fileError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleFileDelete removes a file, or a directory with everything in it.
func (a *Agent) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	path, err := a.filePath(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := os.Lstat(path); err != nil {
		writeError(w, fileError(err))
		return
	}
	if err := os.RemoveAll(path); err != nil {
		writeError(w, fileError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) handleFileList(w http.ResponseWriter, r *http.Request) {
	path, err := a.filePath(r)
	if err != nil {
		writeError(w, err)
		return
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		writeError(w, fileError(err))
		return
	}
	out := make([]agentapi.FileInfo, 0, len(entries))
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			continue // removed while listing
		}
		out = append(out, fileInfo(filepath.Join(path, e.Name()), fi))
	}
	writeJSON(w, http.StatusOK, out)
}

func fileInfo(path string, fi fs.FileInfo) agentapi.FileInfo {
	return agentapi.FileInfo{
		Name:    filepath.Base(path),
		Path:    path,
		Size:    fi.Size(),
		Mode:    unixMode(fi.Mode()),
		IsDir:   fi.IsDir(),
		ModTime: fi.ModTime().UTC(),
	}
}

// handleFileStat serves GET /files/stat?path=P: the FileInfo of one path,
// or 404. A symbolic link is followed, so that is_dir and size describe
// what a read or a listing of the path would find; a link whose target is
// missing is described as the link itself rather than reported absent
// (it is in its directory's listing, and a delete would remove it).
func (a *Agent) handleFileStat(w http.ResponseWriter, r *http.Request) {
	path, err := a.filePath(r)
	if err != nil {
		writeError(w, err)
		return
	}
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if lfi, lerr := os.Lstat(path); lerr == nil {
			fi, err = lfi, nil
		}
	}
	if err != nil {
		writeError(w, fileError(err))
		return
	}
	writeJSON(w, http.StatusOK, fileInfo(path, fi))
}

// unixMode converts to the permission bits chmod(2) uses.
func unixMode(m os.FileMode) uint32 {
	bits := uint32(m.Perm())
	if m&os.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if m&os.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if m&os.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}

func (a *Agent) handleMkdir(w http.ResponseWriter, r *http.Request) {
	path, err := a.filePath(r)
	if err != nil {
		writeError(w, err)
		return
	}
	created, err := mkdirAll(path)
	if err != nil {
		writeError(w, fileError(err))
		return
	}
	if created {
		w.WriteHeader(http.StatusCreated)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mkdirAll is os.MkdirAll that also reports whether this call made the
// directory (false: it was already there). Two concurrent calls for the
// same new path both succeed and exactly one of them reports true.
func mkdirAll(path string) (created bool, err error) {
	err = os.Mkdir(path, 0o755)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrExist):
		fi, serr := os.Stat(path)
		if serr == nil && fi.IsDir() {
			return false, nil
		}
		return false, err // a file is in the way: EEXIST -> 409
	case !errors.Is(err, fs.ErrNotExist):
		return false, err
	}
	// A parent is missing: make them, then the directory itself.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			if fi, serr := os.Stat(path); serr == nil && fi.IsDir() {
				return false, nil // somebody else made it meanwhile
			}
		}
		return false, err
	}
	return true, nil
}
