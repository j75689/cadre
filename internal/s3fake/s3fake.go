// Package s3fake is an in-process S3 server for tests. It implements only what
// Cadre's drivers use: path-style buckets, PutObject with If-None-Match: *,
// GetObject and ListObjectsV2. It is not a general S3 emulator.
package s3fake

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Server is a running fake.
type Server struct {
	*httptest.Server

	mu      sync.Mutex
	buckets map[string]map[string][]byte

	// IgnoreIfNoneMatch makes the fake accept and ignore If-None-Match, like
	// a store that does not enforce it. Set it before use.
	IgnoreIfNoneMatch bool
}

// CreateBucket creates a bucket.
func (s *Server) CreateBucket(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buckets[name] == nil {
		s.buckets[name] = map[string][]byte{}
	}
}

// Keys returns the keys in a bucket.
func (s *Server) Keys(bucket string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.buckets[bucket]))
	for k := range s.buckets[bucket] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// New starts a fake with no buckets. Close it when done.
func New() *Server {
	s := &Server{buckets: map[string]map[string][]byte{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	objects, ok := s.buckets[bucket]
	switch {
	case r.Method == http.MethodPut && key == "":
		if !ok {
			s.buckets[bucket] = map[string][]byte{}
		}
	case !ok:
		writeError(w, http.StatusNotFound, "NoSuchBucket")
	case r.Method == http.MethodPut:
		if _, exists := objects[key]; exists && r.Header.Get("If-None-Match") == "*" && !s.IgnoreIfNoneMatch {
			writeError(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "IncompleteBody")
			return
		}
		objects[key] = body
		w.Header().Set("ETag", `"`+strconv.Itoa(len(body))+`"`)
	case r.Method == http.MethodDelete && key != "":
		delete(objects, key)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && key == "" && r.URL.Query().Get("list-type") == "2":
		list(w, r, bucket, objects)
	case r.Method == http.MethodGet:
		body, exists := objects[key]
		if !exists {
			writeError(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	default:
		writeError(w, http.StatusNotImplemented, "NotImplemented")
	}
}

type listResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	Name                  string
	Prefix                string
	KeyCount              int
	MaxKeys               int
	IsTruncated           bool
	NextContinuationToken string `xml:",omitempty"`
	Contents              []struct {
		Key  string
		Size int
	}
}

func list(w http.ResponseWriter, r *http.Request, bucket string, objects map[string][]byte) {
	q := r.URL.Query()
	prefix, after := q.Get("prefix"), q.Get("start-after")
	if tok := q.Get("continuation-token"); tok != "" {
		after = tok
	}
	maxKeys := 1000
	if v, err := strconv.Atoi(q.Get("max-keys")); err == nil && v > 0 && v < maxKeys {
		maxKeys = v
	}
	var keys []string
	for k := range objects {
		if strings.HasPrefix(k, prefix) && k > after {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	res := listResult{Name: bucket, Prefix: prefix, MaxKeys: maxKeys}
	if len(keys) > maxKeys {
		keys = keys[:maxKeys]
		res.IsTruncated = true
		res.NextContinuationToken = keys[len(keys)-1]
	}
	for _, k := range keys {
		res.Contents = append(res.Contents, struct {
			Key  string
			Size int
		}{k, len(objects[k])})
	}
	res.KeyCount = len(keys)
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(res)
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(struct {
		XMLName xml.Name `xml:"Error"`
		Code    string
	}{Code: code})
}
