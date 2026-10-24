package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/store"
)

// Guardrails for AI agents: tokens with role "agent" may change workloads,
// but destructive changes are held as Approvals until a human approves them.
// Every change made through the API is recorded in the audit log.

const (
	approvalTTL   = 24 * time.Hour     // pending approvals expire
	approvalKeep  = 7 * 24 * time.Hour // decided ones are kept this long
	auditKeep     = 5000               // audit entries kept
	maxGuardBody  = 4 << 20
	agentRunActor = "agentrun/"
)

type approvedKey struct{}

// guardedKinds may only be changed by an agent after approval (CRDs need an
// admin anyway).
var guardedKinds = map[string]bool{
	api.KindSecret: true, api.KindVolume: true, api.KindNetworkPolicy: true,
}

// needsApproval says why an agent's request must wait for a human ("" = it may run).
func needsApproval(r *http.Request, body []byte) string {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodDelete:
		return "deleting needs approval"
	case p == "/api/v1/delete":
		return "deleting needs approval"
	case strings.HasSuffix(p, "/move") && strings.Contains(p, "/volumes/"):
		return "moving a volume needs approval"
	case p == "/api/v1/apply":
		objs, err := api.ParseManifests(body)
		if err != nil {
			return "" // the handler reports the error
		}
		for _, o := range objs {
			if guardedKinds[o.Kind] {
				return "changing a " + o.Kind + " needs approval"
			}
		}
	case strings.Contains(p, "/deployments/") && strings.HasSuffix(p, "/scale"), strings.Contains(p, "/statefulsets/") && strings.HasSuffix(p, "/scale"):
		var b struct {
			Replicas *int `json:"replicas"`
		}
		if json.Unmarshal(body, &b) == nil && b.Replicas != nil && *b.Replicas == 0 {
			return "scaling to zero needs approval"
		}
	}
	return ""
}

// describeRequest summarizes a change in one line for approvals and the audit log.
func describeRequest(method, path string, body []byte) string {
	if path == "/api/v1/apply" || path == "/api/v1/delete" {
		verb := "apply"
		if path == "/api/v1/delete" {
			verb = "delete"
		}
		objs, err := api.ParseManifests(body)
		if err != nil {
			return verb + " (invalid manifest)"
		}
		var names []string
		for _, o := range objs {
			n := strings.ToLower(o.Kind) + "/" + o.Name
			if o.Namespace != "" && o.Namespace != api.DefaultNamespace {
				n = o.Namespace + "/" + n
			}
			names = append(names, n)
		}
		return verb + " " + strings.Join(names, ", ")
	}
	if rest, ok := strings.CutPrefix(path, "/api/v1/namespaces/"); ok {
		parts := strings.Split(rest, "/")
		if len(parts) >= 3 {
			obj := parts[0] + "/" + strings.TrimSuffix(parts[1], "s") + "/" + parts[2]
			switch {
			case method == http.MethodDelete:
				return "delete " + obj
			case len(parts) >= 4:
				extra := ""
				var b map[string]any
				if json.Unmarshal(body, &b) == nil && len(b) > 0 {
					x, _ := json.Marshal(b)
					extra = " " + string(x)
				}
				return parts[3] + " " + obj + extra
			}
		}
	}
	return method + " " + path
}

// guard holds an agent's destructive request as an Approval. It reports
// whether it answered the request.
func (s *Server) guard(w http.ResponseWriter, r *http.Request, id *api.Identity) (*http.Request, bool) {
	if id.Role != api.RoleAgent || r.Context().Value(approvedKey{}) != nil {
		return r, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxGuardBody))
	if err != nil {
		writeErr(w, 400, "reading body: "+err.Error())
		return r, true
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	reason := needsApproval(r, body)
	if reason == "" {
		return r, false
	}
	path := r.URL.Path
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	a := &api.Approval{
		ID: "ap-" + newUID()[:10], Requester: id.Name, RequesterNamespaces: id.Namespaces,
		Summary: describeRequest(r.Method, r.URL.Path, body), Reason: reason, Status: "Pending", CreatedAt: time.Now(),
		Method: r.Method, Path: path, Body: body, ContentType: r.Header.Get("Content-Type"),
	}
	if rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/namespaces/"); ok {
		a.Namespace, _, _ = strings.Cut(rest, "/")
	}
	err = s.store.Write(func(st *store.State) error {
		st.Approvals[a.ID] = a
		st.Event("Warning", "Approval", a.Namespace, a.ID, "ApprovalRequested", id.Name+" asks to "+a.Summary+" ("+reason+")")
		return nil
	})
	if err != nil {
		writeStoreErr(w, err)
		return r, true
	}
	w.Header().Set("X-Nube-Approval", a.ID)
	code, hint := errorCode(http.StatusAccepted)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"error": reason + "; request " + a.ID + " is waiting for a human", "code": code, "hint": hint,
		"approval": a.ID, "summary": a.Summary,
	})
	return r, true
}

// statusRecorder remembers the status code a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = 200
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// audited reports whether a request is a change worth recording.
func audited(r *http.Request, id *api.Identity) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || id.Role == api.RoleNode {
		return false
	}
	p := r.URL.Path
	return !strings.HasPrefix(p, "/api/v1/agent/") && !strings.HasPrefix(p, "/api/v1/cluster/acme") &&
		!strings.HasPrefix(p, "/api/v1/cluster/snapshots") && !strings.HasPrefix(p, "/api/v1/runtime/") &&
		p != "/api/v1/mcp" // its tool calls are recorded one by one
}

func (s *Server) recordAudit(id *api.Identity, method, path string, body []byte, status int, approval string) {
	e := &api.AuditEntry{Time: time.Now(), Who: id.Name, Role: id.Role, Method: method, Path: path, Status: status,
		Summary: describeRequest(method, path, body), Approval: approval}
	s.store.Write(func(st *store.State) error {
		st.AuditSeq++
		e.Seq = st.AuditSeq
		st.Audit[fmt.Sprintf("%012d", e.Seq)] = e
		if old := st.AuditSeq - auditKeep; old > 0 {
			delete(st.Audit, fmt.Sprintf("%012d", old))
		}
		return nil
	})
}

// serveAudited runs a change and records it in the audit log.
func (s *Server) serveAudited(next http.Handler, w http.ResponseWriter, r *http.Request, id *api.Identity) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(io.LimitReader(r.Body, maxGuardBody))
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	rec := &statusRecorder{ResponseWriter: w}
	next.ServeHTTP(rec, r)
	approval, _ := r.Context().Value(approvedKey{}).(string)
	s.recordAudit(id, r.Method, r.URL.Path, body, rec.status, approval)
}

// reconcileApprovals expires old requests.
func reconcileApprovals(st *store.State, now time.Time) {
	for id, a := range st.Approvals {
		switch {
		case a.Status == "Pending" && now.Sub(a.CreatedAt) > approvalTTL:
			a.Status, a.DecidedAt = "Expired", now
		case a.Status != "Pending" && !a.DecidedAt.IsZero() && now.Sub(a.DecidedAt) > approvalKeep:
			delete(st.Approvals, id)
		}
	}
}

// ---------------------------------------------------------------- API

func (s *Server) handleApprovalList(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r)
	status := r.URL.Query().Get("status")
	s.readJSON(w, func(st *store.State) (int, any) {
		out := []api.Approval{}
		for _, a := range st.Approvals {
			if status != "" && !strings.EqualFold(a.Status, status) {
				continue
			}
			if id.Role != api.RoleAdmin && a.Requester != id.Name && !(a.Namespace != "" && nsAllowed(id, a.Namespace) && len(id.Namespaces) > 0) {
				continue
			}
			c := *a
			if id.Role != api.RoleAdmin {
				c.Body = nil // may hold secret values
			}
			out = append(out, c)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
		return 200, map[string]any{"items": out}
	})
}

func (s *Server) handleApprovalGet(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r)
	s.readJSON(w, func(st *store.State) (int, any) {
		a := st.Approvals[r.PathValue("id")]
		if a == nil || id.Role != api.RoleAdmin && a.Requester != id.Name && !strings.HasPrefix(a.Requester, agentRunActor) {
			code, hint := errorCode(404)
			return 404, apiError{Error: "approval " + r.PathValue("id") + " not found", Code: code, Hint: hint}
		}
		c := *a
		if id.Role != api.RoleAdmin {
			c.Body = nil
		}
		return 200, c
	})
}

// handleApprovalDecide approves or rejects a request. An approved API call is
// replayed at once with the requester's identity and its result recorded.
func (s *Server) handleApprovalDecide(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r)
	action := r.PathValue("action")
	if action != "approve" && action != "reject" {
		writeErr(w, 404, "unknown action "+action+" (approve or reject)")
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	var a api.Approval
	err := s.store.Write(func(st *store.State) error {
		cur := st.Approvals[r.PathValue("id")]
		if cur == nil {
			return notFound("approval " + r.PathValue("id"))
		}
		if cur.Status != "Pending" {
			return fmt.Errorf("approval %s is already %s", cur.ID, strings.ToLower(cur.Status))
		}
		cur.Status, cur.DecidedBy, cur.DecidedAt, cur.Note = "Rejected", id.Name, time.Now(), req.Note
		if action == "approve" {
			cur.Status = "Approved"
		}
		st.Event("Normal", "Approval", cur.Namespace, cur.ID, cur.Status, id.Name+" "+strings.ToLower(cur.Status)+": "+cur.Summary)
		a = *cur
		return nil
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if a.Status == "Approved" && a.Method != "" {
		code, body := s.replay(&a)
		a.ResultCode, a.Result = code, body
		a.Status = "Executed"
		if code >= 300 {
			a.Status = "Failed"
		}
		s.store.Write(func(st *store.State) error {
			if cur := st.Approvals[a.ID]; cur != nil {
				cur.Status, cur.ResultCode, cur.Result = a.Status, a.ResultCode, a.Result
			}
			return nil
		})
	}
	a.Body = nil
	writeJSON(w, 200, a)
}

// replay executes an approved API call with the requester's identity.
func (s *Server) replay(a *api.Approval) (int, string) {
	who := &api.Identity{Name: a.Requester, Role: api.RoleAgent, Namespaces: a.RequesterNamespaces}
	req := httptest.NewRequest(a.Method, a.Path, bytes.NewReader(a.Body))
	if a.ContentType != "" {
		req.Header.Set("Content-Type", a.ContentType)
	}
	ctx := context.WithValue(req.Context(), ctxKey{}, who)
	ctx = context.WithValue(ctx, approvedKey{}, a.ID)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	s.recordAudit(who, a.Method, req.URL.Path, a.Body, rec.Code, a.ID)
	out := rec.Body.String()
	if len(out) > 4000 {
		out = out[:4000] + "..."
	}
	return rec.Code, strings.TrimSpace(out)
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit := 200
	fmt.Sscan(r.URL.Query().Get("limit"), &limit)
	who := r.URL.Query().Get("who")
	s.readJSON(w, func(st *store.State) (int, any) {
		keys := make([]string, 0, len(st.Audit))
		for k := range st.Audit {
			keys = append(keys, k)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(keys)))
		out := []api.AuditEntry{}
		for _, k := range keys {
			e := st.Audit[k]
			if who != "" && e.Who != who {
				continue
			}
			out = append(out, *e)
			if len(out) >= limit {
				break
			}
		}
		return 200, map[string]any{"items": out}
	})
}
