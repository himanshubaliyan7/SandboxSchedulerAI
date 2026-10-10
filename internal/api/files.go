package api

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/himanshubaliyan7/warmbox/internal/agentapi"
	"github.com/himanshubaliyan7/warmbox/internal/agentclient"
)

// File routes. Paths are interpreted by the in-sandbox agent: absolute,
// or relative to the sandbox's work directory. Bodies are streamed in
// both directions and never held in gateway memory.

func filePath(r *http.Request) (string, error) {
	p := r.URL.Query().Get("path")
	if p == "" {
		return "", badRequest("the path query parameter is required")
	}
	return p, nil
}

func (s *Server) handleFileRead(w http.ResponseWriter, r *http.Request, sess *session) error {
	path, err := filePath(r)
	if err != nil {
		return err
	}
	ctx, cancel := s.streamCtx(r)
	defer cancel()
	body, size, err := sess.client.OpenFile(ctx, path)
	if err != nil {
		return s.agentFailure(sess, err, CodeFileNotFound)
	}
	defer body.Close()
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if size >= 0 {
		h.Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil {
		// The status line is gone; abort the response so the client
		// sees a truncated body rather than a short file that looks whole.
		panic(http.ErrAbortHandler)
	}
	return nil
}

func (s *Server) handleFileWrite(w http.ResponseWriter, r *http.Request, sess *session) error {
	path, err := filePath(r)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if v := r.URL.Query().Get("mode"); v != "" {
		m, err := strconv.ParseUint(v, 8, 32)
		if err != nil || m > 0o777 {
			return badRequest("mode must be octal permission bits, e.g. 0644")
		}
		mode = os.FileMode(m)
	}
	if r.ContentLength > s.cfg.MaxUploadBytes {
		return errf(http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "upload exceeds %d bytes", s.cfg.MaxUploadBytes)
	}
	ctx, cancel := s.streamCtx(r)
	defer cancel()
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	if err := sess.client.WriteFile(ctx, path, mode, body); err != nil {
		return s.agentFailure(sess, bodyError(err), CodeFileNotFound)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request, sess *session) error {
	path, err := filePath(r)
	if err != nil {
		return err
	}
	ctx, cancel := s.opCtx(r)
	defer cancel()
	if err := sess.client.DeleteFile(ctx, path); err != nil {
		return s.agentFailure(sess, err, CodeFileNotFound)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// FileView is one directory entry.
type FileView struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"` // octal permission bits, e.g. "0644"
	IsDir   bool   `json:"is_dir"`
	ModTime string `json:"mod_time"`
}

func fileView(f agentapi.FileInfo) FileView {
	return FileView{Name: f.Name, Path: f.Path, Size: f.Size, Mode: "0" + strconv.FormatUint(uint64(f.Mode&0o777), 8),
		IsDir: f.IsDir, ModTime: f.ModTime.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")}
}

func (s *Server) handleFileList(w http.ResponseWriter, r *http.Request, sess *session) error {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "."
	}
	ctx, cancel := s.opCtx(r)
	defer cancel()
	files, err := sess.client.ListFiles(ctx, path)
	if err != nil {
		return s.agentFailure(sess, err, CodeFileNotFound)
	}
	out := make([]FileView, 0, len(files))
	for _, f := range files {
		out = append(out, fileView(f))
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": out})
	return nil
}

// handleFileStat describes one path: GET .../files/stat?path=P. It is how
// a client asks "does this exist, and is it a directory" without reading
// or listing anything.
func (s *Server) handleFileStat(w http.ResponseWriter, r *http.Request, sess *session) error {
	path, err := filePath(r)
	if err != nil {
		return err
	}
	ctx, cancel := s.opCtx(r)
	defer cancel()
	fi, err := sess.client.StatFile(ctx, path)
	if err != nil {
		var age *agentclient.Error
		if errors.As(err, &age) && age.Code == "no_route" {
			// A warm sandbox created by an older server and adopted by
			// this one still runs the agent it was created with.
			return errf(http.StatusNotImplemented, CodeNotImplemented,
				"this sandbox's agent predates the stat route; list the parent directory instead")
		}
		return s.agentFailure(sess, err, CodeFileNotFound)
	}
	writeJSON(w, http.StatusOK, fileView(fi))
	return nil
}

// handleMkdir creates a directory and its missing parents. 201 means
// this request created it, 204 that it was already there; both are
// success, and a client that does not care treats any 2xx alike (204 was
// the only answer before the distinction existed).
func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request, sess *session) error {
	path, err := filePath(r)
	if err != nil {
		return err
	}
	ctx, cancel := s.opCtx(r)
	defer cancel()
	created, err := sess.client.Mkdir(ctx, path)
	if err != nil {
		return s.agentFailure(sess, err, CodeFileNotFound)
	}
	if created {
		w.WriteHeader(http.StatusCreated)
		return nil
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
