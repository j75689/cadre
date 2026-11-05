// Package s3fake is an in-process S3 server for tests. It implements only what
// Cadre's drivers use: path-style buckets, PutObject with If-None-Match: *,
// GetObject, HeadObject, DeleteObject and ListObjectsV2 (prefix, delimiter, start-after,
// max-keys and continuation). It is not a general S3 emulator.
package s3fake

import (
	"encoding/xml"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Server is a running fake.
type Server struct {
	*httptest.Server

	mu      sync.Mutex
	buckets map[string]map[string][]byte
	dirs    map[string]map[string]bool // with Filer, per bucket

	// IgnoreIfNoneMatch makes the fake accept and ignore If-None-Match, like
	// a store that does not enforce it. Set it before use.
	IgnoreIfNoneMatch bool

	// Filer makes the fake behave like a store backed by a filesystem
	// (SeaweedFS): a key that is also the directory of other keys cannot
	// hold an object, so a conditional put of it answers 412 and a HEAD of
	// it answers 404. Its directories ("…/") outlive their objects: a
	// listing with a delimiter reports one as a common prefix, and a DELETE
	// of an empty one removes it. Set it before use.
	Filer bool

	// HeadFailures makes the next that many HEAD requests fail with 500.
	HeadFailures atomic.Int32
}

// CreateBucket creates a bucket.
func (s *Server) CreateBucket(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buckets[name] == nil {
		s.buckets[name] = map[string][]byte{}
		s.dirs[name] = map[string]bool{}
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
	s := &Server{buckets: map[string]map[string][]byte{}, dirs: map[string]map[string]bool{}}
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
			s.dirs[bucket] = map[string]bool{}
		}
	case !ok:
		writeError(w, http.StatusNotFound, "NoSuchBucket")
	case r.Method == http.MethodPut:
		conditional := r.Header.Get("If-None-Match") == "*" && !s.IgnoreIfNoneMatch
		if _, exists := objects[key]; conditional && (exists || s.Filer && isDir(objects, key)) {
			writeError(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "IncompleteBody")
			return
		}
		objects[key] = body
		if s.Filer {
			for i := range key {
				if key[i] == '/' {
					s.dirs[bucket][key[:i+1]] = true
				}
			}
		}
		w.Header().Set("ETag", `"`+strconv.Itoa(len(body))+`"`)
	case r.Method == http.MethodDelete && key != "":
		delete(objects, key)
		if strings.HasSuffix(key, "/") && !holds(objects, s.dirs[bucket], key) {
			delete(s.dirs[bucket], key)
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && key == "" && r.URL.Query().Get("list-type") == "2":
		list(w, r, bucket, objects, s.dirs[bucket])
	case r.Method == http.MethodHead && s.takeHeadFailure():
		writeError(w, http.StatusInternalServerError, "InternalError")
	case r.Method == http.MethodHead:
		body, exists := objects[key]
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
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
	CommonPrefixes []struct{ Prefix string }
}

func list(w http.ResponseWriter, r *http.Request, bucket string, objects map[string][]byte, dirs map[string]bool) {
	q := r.URL.Query()
	prefix, after, delim := q.Get("prefix"), q.Get("start-after"), q.Get("delimiter")
	if tok := q.Get("continuation-token"); tok != "" {
		after = tok
	}
	maxKeys := 1000
	if v, err := strconv.Atoi(q.Get("max-keys")); err == nil && v > 0 && v < maxKeys {
		maxKeys = v
	}
	var keys []string
	prefixes := map[string]bool{}
	// With a delimiter, keys and directories below the next delimiter roll
	// up into CommonPrefixes, all returned on each page.
	rollUp := func(k string) bool {
		rest, ok := strings.CutPrefix(k, prefix)
		i := strings.Index(rest, delim)
		if !ok || delim == "" || i < 0 {
			return false
		}
		if p := prefix + rest[:i+len(delim)]; p > after {
			prefixes[p] = true
		}
		return true
	}
	for k := range objects {
		if !rollUp(k) && strings.HasPrefix(k, prefix) && k > after {
			keys = append(keys, k)
		}
	}
	for d := range dirs {
		rollUp(d)
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
	for _, p := range slices.Sorted(maps.Keys(prefixes)) {
		res.CommonPrefixes = append(res.CommonPrefixes, struct{ Prefix string }{p})
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

// holds reports whether dir has an object or a directory below it.
func holds(objects map[string][]byte, dirs map[string]bool, dir string) bool {
	for k := range objects {
		if strings.HasPrefix(k, dir) {
			return true
		}
	}
	for d := range dirs {
		if d != dir && strings.HasPrefix(d, dir) {
			return true
		}
	}
	return false
}

// isDir reports whether key is the directory of another key.
func isDir(objects map[string][]byte, key string) bool {
	for k := range objects {
		if strings.HasPrefix(k, key+"/") {
			return true
		}
	}
	return false
}

func (s *Server) takeHeadFailure() bool {
	for {
		n := s.HeadFailures.Load()
		if n <= 0 {
			return false
		}
		if s.HeadFailures.CompareAndSwap(n, n-1) {
			return true
		}
	}
}
