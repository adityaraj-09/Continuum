// Package gitserver exposes the system Git smart-HTTP implementation.
package gitserver

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/adityaraj-09/Continuum/engine/internal/repository"
)

type Handler struct {
	ReposDir string
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/git/") {
		http.NotFound(w, r)
		return
	}
	pathInfo := strings.TrimPrefix(r.URL.Path, "/git")
	repoPart := strings.TrimPrefix(pathInfo, "/")
	repoID, _, ok := strings.Cut(repoPart, ".git")
	if !ok || repository.ValidateID(repoID) != nil {
		http.Error(w, "invalid repository path", http.StatusBadRequest)
		return
	}

	cmd := exec.CommandContext(r.Context(), "git", "http-backend")
	cmd.Env = append(os.Environ(),
		"GIT_PROJECT_ROOT="+h.ReposDir,
		"GIT_HTTP_EXPORT_ALL=1",
		"PATH_INFO="+pathInfo,
		"QUERY_STRING="+r.URL.RawQuery,
		"REQUEST_METHOD="+r.Method,
		"CONTENT_TYPE="+r.Header.Get("Content-Type"),
		"CONTENT_LENGTH="+strconv.FormatInt(r.ContentLength, 10),
		"REMOTE_ADDR="+r.RemoteAddr,
		"HTTP_GIT_PROTOCOL="+r.Header.Get("Git-Protocol"),
	)
	cmd.Stdin = r.Body
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	reader := bufio.NewReader(stdout)
	headers, err := readCGIHeaders(reader)
	if err != nil {
		_ = cmd.Wait()
		http.Error(w, fmt.Sprintf("git http-backend: %v: %s", err, stderr.String()), http.StatusBadGateway)
		return
	}
	status := http.StatusOK
	for key, values := range headers {
		if strings.EqualFold(key, "Status") {
			if fields := strings.Fields(values[0]); len(fields) > 0 {
				if parsed, parseErr := strconv.Atoi(fields[0]); parseErr == nil {
					status = parsed
				}
			}
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(status)
	_, _ = io.Copy(w, reader)
	if err := cmd.Wait(); err != nil {
		// The response may already contain Git's protocol-level rejection.
		return
	}
}

func readCGIHeaders(r *bufio.Reader) (http.Header, error) {
	headers := make(http.Header)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return headers, nil
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("invalid CGI header %q", line)
		}
		headers.Add(strings.TrimSpace(key), strings.TrimSpace(value))
	}
}
