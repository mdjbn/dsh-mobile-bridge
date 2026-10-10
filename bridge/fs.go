package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// maxFsReadBytes caps a single /v1/fs/get response at 8 MiB, per the contract.
const maxFsReadBytes = 8 << 20

// fsManager owns filesystem access on this PC. It is stateless: every check
// runs against the live config, so a config reload cannot leave stale rights.
type fsManager struct{}

type fsPutRequest struct {
	Path   string `json:"path"`
	Data   string `json:"data"`
	Offset int64  `json:"offset"`
	Append bool   `json:"append"`
	Mkdirs bool   `json:"mkdirs"`
}

type fsPutResponse struct {
	Path         string `json:"path"`
	BytesWritten int    `json:"bytesWritten"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
}

type fsGetRequest struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
}

type fsGetResponse struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Data   string `json:"data"`
	Bytes  int    `json:"bytes"`
	Size   int64  `json:"size"`
	EOF    bool   `json:"eof"`
	SHA256 string `json:"sha256"`
}

type fsLsRequest struct {
	Path string `json:"path"`
}

type fsEntry struct {
	Name     string `json:"name"`
	Dir      bool   `json:"dir"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
}

type fsLsResponse struct {
	Path    string    `json:"path"`
	Entries []fsEntry `json:"entries"`
}

// handleFsPut writes a (possibly chunked) file on this PC. `append` and
// `offset` let a caller stream a large image without holding it in memory.
func (a *API) handleFsPut(w http.ResponseWriter, r *http.Request) {
	if !a.requirePost(w, r) {
		return
	}
	var req fsPutRequest
	if err := decodeBody(r.Body, &req); err != nil {
		writeBadRequest(w, err)
		return
	}
	if err := a.cfg.checkAllowed(req.Path); err != nil {
		writeFsError(w, err)
		return
	}
	payload, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		writeBadRequest(w, fmt.Errorf("data is not valid base64: %v", err))
		return
	}
	path := filepath.Clean(req.Path)
	if req.Mkdirs {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			writeBadRequest(w, fmt.Errorf("cannot create parent directory: %v", err))
			return
		}
	}
	flags := os.O_WRONLY | os.O_CREATE
	switch {
	case req.Append:
		flags |= os.O_APPEND
	case req.Offset <= 0:
		flags |= os.O_TRUNC
	}
	file, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		writeBadRequest(w, fmt.Errorf("cannot open %s: %v", path, err))
		return
	}
	if !req.Append && req.Offset > 0 {
		if _, err := file.Seek(req.Offset, io.SeekStart); err != nil {
			file.Close()
			writeBadRequest(w, fmt.Errorf("cannot seek to %d: %v", req.Offset, err))
			return
		}
	}
	written, writeErr := file.Write(payload)
	closeErr := file.Close()
	if writeErr != nil {
		writeBadRequest(w, fmt.Errorf("write failed: %v", writeErr))
		return
	}
	if closeErr != nil {
		writeBadRequest(w, fmt.Errorf("close failed: %v", closeErr))
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		writeBadRequest(w, fmt.Errorf("cannot stat %s: %v", path, err))
		return
	}
	digest, err := fileSHA256(path)
	if err != nil {
		writeBadRequest(w, fmt.Errorf("cannot hash %s: %v", path, err))
		return
	}
	writeJSON(w, http.StatusOK, fsPutResponse{Path: path, BytesWritten: written, Size: info.Size(), SHA256: digest})
}

// handleFsGet reads a bounded slice of a file. `sha256` always covers the
// whole file, so a chunked download can verify the assembled result.
func (a *API) handleFsGet(w http.ResponseWriter, r *http.Request) {
	if !a.requirePost(w, r) {
		return
	}
	var req fsGetRequest
	if err := decodeBody(r.Body, &req); err != nil {
		writeBadRequest(w, err)
		return
	}
	if err := a.cfg.checkAllowed(req.Path); err != nil {
		writeFsError(w, err)
		return
	}
	path := filepath.Clean(req.Path)
	if req.Offset < 0 {
		writeBadRequest(w, fmt.Errorf("offset must not be negative"))
		return
	}
	length := req.Length
	if length <= 0 || length > maxFsReadBytes {
		length = maxFsReadBytes
	}
	file, err := os.Open(path)
	if err != nil {
		writeFsError(w, fmt.Errorf("cannot open %s: %w", path, err))
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeFsError(w, fmt.Errorf("cannot stat %s: %w", path, err))
		return
	}
	if req.Offset > info.Size() {
		writeBadRequest(w, fmt.Errorf("offset %d is past the end of %s (%d bytes)", req.Offset, path, info.Size()))
		return
	}
	if _, err := file.Seek(req.Offset, io.SeekStart); err != nil {
		writeBadRequest(w, fmt.Errorf("cannot seek to %d: %v", req.Offset, err))
		return
	}
	buf := make([]byte, length)
	read, err := io.ReadFull(file, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		writeBadRequest(w, fmt.Errorf("read failed: %v", err))
		return
	}
	buf = buf[:read]
	digest, err := fileSHA256(path)
	if err != nil {
		writeBadRequest(w, fmt.Errorf("cannot hash %s: %v", path, err))
		return
	}
	writeJSON(w, http.StatusOK, fsGetResponse{
		Path:   path,
		Offset: req.Offset,
		Data:   base64.StdEncoding.EncodeToString(buf),
		Bytes:  read,
		Size:   info.Size(),
		EOF:    req.Offset+int64(read) >= info.Size(),
		SHA256: digest,
	})
}

// handleFsLs lists one directory on this PC.
func (a *API) handleFsLs(w http.ResponseWriter, r *http.Request) {
	if !a.requirePost(w, r) {
		return
	}
	var req fsLsRequest
	if err := decodeBody(r.Body, &req); err != nil {
		writeBadRequest(w, err)
		return
	}
	if err := a.cfg.checkAllowed(req.Path); err != nil {
		writeFsError(w, err)
		return
	}
	path := filepath.Clean(req.Path)
	dirents, err := os.ReadDir(path)
	if err != nil {
		writeFsError(w, fmt.Errorf("cannot list %s: %w", path, err))
		return
	}
	entries := make([]fsEntry, 0, len(dirents))
	for _, dirent := range dirents {
		entry := fsEntry{Name: dirent.Name(), Dir: dirent.IsDir()}
		if info, err := dirent.Info(); err == nil {
			entry.Size = info.Size()
			entry.Modified = info.ModTime().UTC().Format(time.RFC3339)
		}
		entries = append(entries, entry)
	}
	writeJSON(w, http.StatusOK, fsLsResponse{Path: path, Entries: entries})
}

// fileSHA256 hashes a whole file in one streaming pass.
func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
