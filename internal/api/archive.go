package api

import (
	"fmt"
	"net/http"
	"regexp"

	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
)

// snapshotUID is what a snapshot's uid may be: a Task's uid, and never a
// path.
var snapshotUID = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// listArchive answers a page of a namespace's archived tasks, newest
// first. A namespace without a Team is not found.
func (s *Server) listArchive(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	if _, err := s.configTeam(r.Context(), ns); err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	query := r.URL.Query()
	for _, name := range []string{"offset", "limit"} {
		if n := len(query[name]); n > 1 {
			httpjson.WriteError(w, http.StatusBadRequest, httpjson.CodeInvalid, fmt.Sprintf("%s given %d times: at most once", name, n))
			return
		}
	}
	offset, ok := intParam(w, query, "offset")
	if !ok {
		return
	}
	limit, ok := intParam(w, query, "limit")
	if !ok {
		return
	}
	limit = ClampLimit(limit)
	items, total, err := s.m.ListSnapshots(r.Context(), ns, offset, limit)
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	httpjson.Write(w, http.StatusOK, ArchiveList{Items: items, Offset: offset, Limit: limit, Total: total})
}

// getArchive answers an archived task's snapshot, as stored, so the
// provider's JSON in its transcripts is as it came.
func (s *Server) getArchive(w http.ResponseWriter, r *http.Request) {
	ns, uid := r.PathValue("ns"), r.PathValue("uid")
	if _, err := s.configTeam(r.Context(), ns); err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	if !snapshotUID.MatchString(uid) {
		httpjson.Fail(w, s.log, fmt.Errorf("uid %q must be 1 to 64 letters, digits and dashes: %w", uid, manager.ErrInvalid))
		return
	}
	body, err := s.m.GetSnapshot(r.Context(), ns, uid)
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	httpjson.WriteVerbatim(w, http.StatusOK, body)
}
