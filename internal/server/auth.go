package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/store"
)

// Tokens look like "nube_<id>_<secret>"; only sha256(token) is stored.
const tokenPrefix = "nube_"

type ctxKey struct{}

func identityFrom(r *http.Request) *api.Identity {
	if id, ok := r.Context().Value(ctxKey{}).(*api.Identity); ok {
		return id
	}
	return &api.Identity{}
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func bearer(r *http.Request) string {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		tok = r.URL.Query().Get("token") // browser log streaming
	}
	return strings.TrimSpace(tok)
}

// authenticate maps a request's token to an identity.
func (s *Server) authenticate(r *http.Request) *api.Identity {
	tok := bearer(r)
	if tok == "" {
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.Token)) == 1 {
		return &api.Identity{Name: "cluster-admin", Role: api.RoleAdmin}
	}
	if strings.HasPrefix(tok, runTokenPrefix) {
		var ident *api.Identity
		s.store.Read(func(st *store.State) { ident = runIdentity(st, tok) })
		return ident
	}
	if !strings.HasPrefix(tok, tokenPrefix) {
		return nil
	}
	id, _, ok := strings.Cut(strings.TrimPrefix(tok, tokenPrefix), "_")
	if !ok {
		return nil
	}
	var ident *api.Identity
	s.store.Read(func(st *store.State) {
		t := st.Tokens[id]
		if t != nil && subtle.ConstantTimeCompare([]byte(hashToken(tok)), []byte(t.Hash)) == 1 {
			ident = &api.Identity{Name: t.Name, Role: t.Role, Namespaces: append([]string(nil), t.Namespaces...)}
		}
	})
	return ident
}

// revealRole is the requirement for reading secret values: edit or admin.
const revealRole = "reveal"

var roleRank = map[string]int{api.RoleView: 1, api.RoleEdit: 2, api.RoleAgent: 2, api.RoleAdmin: 3}

// requiredRole decides the minimum role for a request.
func requiredRole(r *http.Request) string {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/api/v1/agent/"):
		return api.RoleNode
	case strings.HasPrefix(p, "/api/v1/runtime/"):
		return api.RoleRun
	case p == "/api/v1/mcp":
		return api.RoleView // each tool call is checked on its own
	case p == "/api/v1/version" || p == "/api/v1/whoami":
		return "" // any authenticated identity
	case strings.HasPrefix(p, "/api/v1/tokens"), strings.HasPrefix(p, "/api/v1/cluster/"), strings.HasPrefix(p, "/api/v1/audit"):
		return api.RoleAdmin
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		if r.URL.Query().Get("reveal") == "1" {
			return revealRole
		}
		return api.RoleView
	case strings.HasPrefix(p, "/api/v1/nodes"):
		return api.RoleAdmin
	case p == "/api/v1/apply", p == "/api/v1/delete", strings.HasPrefix(p, "/api/v1/namespaces/"):
		return api.RoleEdit
	}
	return api.RoleAdmin
}

func allowed(id *api.Identity, need string) bool {
	switch {
	case need == "":
		return true
	case need == revealRole:
		return id.Role == api.RoleEdit || id.Role == api.RoleAdmin // agents never see secret values
	case need == api.RoleNode:
		return id.Role == api.RoleNode || id.Role == api.RoleAdmin
	case need == api.RoleRun:
		return id.Role == api.RoleRun
	case id.Role == api.RoleNode, id.Role == api.RoleRun:
		return false
	}
	return roleRank[id.Role] >= roleRank[need]
}

// nsAllowed reports whether the identity may touch a namespace.
func nsAllowed(id *api.Identity, ns string) bool {
	return len(id.Namespaces) == 0 || slices.Contains(id.Namespaces, ns)
}

// authorize authenticates the request and checks role and namespace. It
// returns the request carrying the identity, or writes an error.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	id := s.authenticate(r)
	if id == nil {
		writeErr(w, http.StatusUnauthorized, "missing or invalid token")
		return nil, false
	}
	if msg := accessDenied(id, r); msg != "" {
		writeErr(w, http.StatusForbidden, msg)
		return nil, false
	}
	return r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)), true
}

// accessDenied checks role and namespace for a request ("" = allowed).
func accessDenied(id *api.Identity, r *http.Request) string {
	if need := requiredRole(r); !allowed(id, need) {
		if need == revealRole {
			need = "edit"
		}
		return fmt.Sprintf("%s (role %s) may not do this; needs role %s", id.Name, id.Role, need)
	}
	if rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/namespaces/"); ok {
		ns, _, _ := strings.Cut(rest, "/")
		if !nsAllowed(id, ns) {
			return fmt.Sprintf("%s may not access namespace %s", id.Name, ns)
		}
	}
	return ""
}

// ---------------------------------------------------------------- token API

func (s *Server) handleTokenList(w http.ResponseWriter, r *http.Request) {
	s.readJSON(w, func(st *store.State) (int, any) {
		out := []api.Token{}
		for _, t := range st.Tokens {
			c := *t
			c.Hash = ""
			out = append(out, c)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return 200, map[string]any{"items": out}
	})
}

func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	var req api.Token
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "body must be {\"name\":..., \"role\":..., \"namespaces\":[...]}")
		return
	}
	if !api.ValidName(req.Name) {
		writeErr(w, 400, "token name must be lowercase letters, digits and '-'")
		return
	}
	switch req.Role {
	case api.RoleAdmin, api.RoleEdit, api.RoleView, api.RoleNode, api.RoleAgent:
	default:
		writeErr(w, 400, "role must be admin, edit, agent, view or node")
		return
	}
	for _, ns := range req.Namespaces {
		if !api.ValidName(ns) {
			writeErr(w, 400, "invalid namespace "+ns)
			return
		}
	}
	id := newUID()[:8]
	secret := newUID() + newUID()
	full := tokenPrefix + id + "_" + secret
	t := &api.Token{ID: id, Name: req.Name, Role: req.Role, Namespaces: req.Namespaces, Hash: hashToken(full), CreatedAt: time.Now()}
	err := s.store.Write(func(st *store.State) error {
		for _, o := range st.Tokens {
			if o.Name == req.Name {
				return fmt.Errorf("a token named %s already exists", req.Name)
			}
		}
		st.Tokens[id] = t
		st.Event("Normal", api.KindToken, "", req.Name, "Created", "token created with role "+req.Role)
		return nil
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	out := *t
	out.Hash, out.Secret = "", full
	writeJSON(w, 200, out)
}

func (s *Server) handleTokenDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := s.store.Write(func(st *store.State) error {
		for id, t := range st.Tokens {
			if t.Name == name || t.ID == name {
				delete(st.Tokens, id)
				st.Event("Normal", api.KindToken, "", t.Name, "Revoked", "token revoked")
				return nil
			}
		}
		return notFound("token/" + name)
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "token " + name + " revoked"})
}
