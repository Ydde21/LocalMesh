package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/creds"
	"github.com/localmesh/localmesh/internal/release"
	"github.com/localmesh/localmesh/internal/server/auth"
	"github.com/localmesh/localmesh/internal/server/store"
	"github.com/localmesh/localmesh/internal/server/updateplane"
)

// ---------------------------------------------------------------------------
// Release registry + agent-update orchestration endpoints.
//
// Admin surface: signing keys, releases (verified at registration by the
// updateplane manager), artifact uploads into the control-plane store,
// rollout lifecycle (create/approve/cancel), per-node update rows.
//
// Agent surface: GET /api/v1/agent-releases/{id}/files/{name} — the
// localmesh:// proxy backend, authenticated by NODE credential (lmn_).
// The agent verifies signatures itself; this endpoint is transport.
// ---------------------------------------------------------------------------

// artifactStore is the filesystem store the server wires in — declared
// here so adminapi stays free of the server's concrete type.
type artifactStore interface {
	Put(ctx context.Context, rel *store.Release, name string, r io.Reader) (string, int64, error)
	PutBytes(rel *store.Release, name string, data []byte) error
	Open(rel *store.Release, name string) (io.ReadSeekCloser, int64, error)
}

func (a *API) registerReleases(mux *http.ServeMux, mwAdmin, mwRead func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v1/release-keys", mwRead(http.HandlerFunc(a.handleListReleaseKeys)))
	mux.Handle("POST /api/v1/release-keys", mwAdmin(http.HandlerFunc(a.handleCreateReleaseKey)))
	mux.Handle("POST /api/v1/release-keys/{id}/revoke", mwAdmin(http.HandlerFunc(a.handleRevokeReleaseKey)))

	mux.Handle("GET /api/v1/releases", mwRead(http.HandlerFunc(a.handleListReleases)))
	mux.Handle("GET /api/v1/releases/{id}", mwRead(http.HandlerFunc(a.handleGetRelease)))
	mux.Handle("POST /api/v1/releases", mwAdmin(http.HandlerFunc(a.handleRegisterRelease)))
	mux.Handle("POST /api/v1/releases/{id}/revoke", mwAdmin(http.HandlerFunc(a.handleRevokeRelease)))
	mux.Handle("DELETE /api/v1/releases/{id}", mwAdmin(http.HandlerFunc(a.handleRevokeRelease)))
	mux.Handle("PUT /api/v1/releases/{id}/artifacts/{name}",
		mwAdmin(http.HandlerFunc(a.handleUploadArtifact)))

	mux.Handle("GET /api/v1/rollouts", mwRead(http.HandlerFunc(a.handleListRollouts)))
	mux.Handle("GET /api/v1/rollouts/{id}", mwRead(http.HandlerFunc(a.handleGetRollout)))
	mux.Handle("POST /api/v1/rollouts", mwAdmin(http.HandlerFunc(a.handleCreateRollout)))
	mux.Handle("POST /api/v1/rollouts/{id}/approve", mwAdmin(http.HandlerFunc(a.handleApproveRollout)))
	mux.Handle("POST /api/v1/rollouts/{id}/cancel", mwAdmin(http.HandlerFunc(a.handleCancelRollout)))

	mux.Handle("GET /api/v1/agent-updates", mwRead(http.HandlerFunc(a.handleListAgentUpdates)))
	mux.Handle("GET /api/v1/agent-updates/{id}", mwRead(http.HandlerFunc(a.handleGetAgentUpdate)))
	mux.Handle("POST /api/v1/agent-updates/{id}/cancel", mwAdmin(http.HandlerFunc(a.handleCancelAgentUpdate)))

	// Node-authenticated artifact proxy — deliberately NOT behind the
	// admin middleware: the agent bears lmn_ credentials.
	mux.HandleFunc("GET /api/v1/agent-releases/{id}/files/{name}", a.handleAgentReleaseFile)
}

// ---- release keys -----------------------------------------------------------

func (a *API) handleListReleaseKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := a.updates.ListReleaseKeys(r.Context())
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	if keys == nil {
		keys = []*store.ReleaseKey{}
	}
	writeJSON(w, http.StatusOK, keys)
}

func (a *API) handleCreateReleaseKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PublicKey string `json:"public_key"`
		Comment   string `json:"comment"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.PublicKey == "" {
		writeErr(w, http.StatusBadRequest, "public_key is required")
		return
	}
	k, err := a.updates.RegisterKey(r.Context(), []byte(req.PublicKey), req.Comment)
	if err != nil {
		if errors.Is(err, store.ErrDuplicateKey) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	principal, _ := auth.PrincipalFrom(r.Context())
	a.st.Audit(r.Context(), principal.Name, "release_key.register", k.KeyID, nil)
	writeJSON(w, http.StatusCreated, k)
}

func (a *API) handleRevokeReleaseKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.updates.RevokeReleaseKey(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "key not found")
			return
		}
		a.internalErr(w, r, err)
		return
	}
	principal, _ := auth.PrincipalFrom(r.Context())
	a.st.Audit(r.Context(), principal.Name, "release_key.revoke", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

// ---- releases ---------------------------------------------------------------

func (a *API) handleListReleases(w http.ResponseWriter, r *http.Request) {
	rels, err := a.updates.ListReleases(r.Context())
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	if rels == nil {
		rels = []*store.Release{}
	}
	writeJSON(w, http.StatusOK, rels)
}

func (a *API) handleGetRelease(w http.ResponseWriter, r *http.Request) {
	rel, err := a.updates.GetRelease(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "release not found")
		return
	}
	writeJSON(w, http.StatusOK, rel)
}

func (a *API) handleRegisterRelease(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ManifestJSON string            `json:"manifest_json"` // verbatim release-manifest.json
		Sums         string            `json:"sha256sums"`    // verbatim SHA256SUMS
		Signatures   map[string]string `json:"signatures"`    // "<file>.minisig"|".sig" → contents
		BaseURL      string            `json:"base_url"`
		Channel      string            `json:"channel"`
		Notes        string            `json:"notes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.ManifestJSON == "" || req.Sums == "" || len(req.Signatures) == 0 {
		writeErr(w, http.StatusBadRequest, "manifest_json, sha256sums and signatures are required")
		return
	}
	principal, _ := auth.PrincipalFrom(r.Context())
	rel, err := a.updates.RegisterRelease(r.Context(), updateplane.RegisterInput{
		ManifestJSON: []byte(req.ManifestJSON),
		SumsJSON:     []byte(req.Sums),
		Signatures:   req.Signatures,
		BaseURL:      req.BaseURL,
		Channel:      req.Channel,
		Notes:        req.Notes,
		CreatedBy:    principal.Name,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrDuplicateRelease):
			writeErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, updateplane.ErrUnsignedRelease),
			errors.Is(err, updateplane.ErrInvalidRollout):
			writeErr(w, http.StatusBadRequest, err.Error())
		default:
			writeErr(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	// Store the verified release metadata verbatim — the agent proxy
	// serves these bytes back and agents re-verify signatures.
	if a.artifacts != nil {
		for name, contents := range map[string]string{
			release.ManifestFile: req.ManifestJSON,
			release.SumsFile:     req.Sums,
		} {
			if err := a.artifacts.PutBytes(rel, name, []byte(contents)); err != nil {
				a.log.Warn("release metadata store failed — proxy will 404",
					"release", rel.ID, "file", name, "err", err)
			}
		}
		for name, contents := range req.Signatures {
			if err := a.artifacts.PutBytes(rel, name, []byte(contents)); err != nil {
				a.log.Warn("release signature store failed",
					"release", rel.ID, "file", name, "err", err)
			}
		}
	}
	a.st.Audit(r.Context(), principal.Name, "release.register", rel.ID,
		map[string]string{"version": rel.Version, "channel": rel.Channel})
	writeJSON(w, http.StatusCreated, rel)
}

func (a *API) handleRevokeRelease(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.updates.RevokeRelease(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "release not found")
			return
		}
		a.internalErr(w, r, err)
		return
	}
	principal, _ := auth.PrincipalFrom(r.Context())
	a.st.Audit(r.Context(), principal.Name, "release.revoke", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

// handleUploadArtifact receives one binary artifact for a registered
// release. The store verifies the bytes' sha256 against the signed
// manifest before committing — a bad upload never becomes servable.
func (a *API) handleUploadArtifact(w http.ResponseWriter, r *http.Request) {
	if a.artifacts == nil {
		writeErr(w, http.StatusNotFound, "artifact store unavailable")
		return
	}
	rel, err := a.updates.GetRelease(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "release not found")
		return
	}
	name := r.PathValue("name")
	sum, size, err := a.artifacts.Put(r.Context(), rel, name,
		http.MaxBytesReader(w, r.Body, 5<<30))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	principal, _ := auth.PrincipalFrom(r.Context())
	a.st.Audit(r.Context(), principal.Name, "release.artifact", rel.ID,
		map[string]string{"file": name, "sha256": sum, "bytes": strconv.FormatInt(size, 10)})
	writeJSON(w, http.StatusCreated, map[string]any{
		"file": name, "sha256": sum, "size_bytes": size,
	})
}

// ---- rollouts ---------------------------------------------------------------

func (a *API) handleListRollouts(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	ros, err := a.updates.ListRollouts(r.Context(), limit)
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	if ros == nil {
		ros = []*store.Rollout{}
	}
	writeJSON(w, http.StatusOK, ros)
}

func (a *API) handleGetRollout(w http.ResponseWriter, r *http.Request) {
	ro, err := a.updates.GetRollout(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "rollout not found")
		return
	}
	ups, err := a.st.ListAgentUpdates(r.Context(), ro.ID, "", "", 500)
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rollout": ro, "updates": ups})
}

func (a *API) handleCreateRollout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ReleaseID   string   `json:"release_id"`
		Strategy    string   `json:"strategy"` // single|selected|sequential|canary|parallel
		NodeIDs     []string `json:"node_ids"`
		CanaryCount int      `json:"canary_count"`
		BatchSize   int      `json:"batch_size"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.ReleaseID == "" || req.Strategy == "" {
		writeErr(w, http.StatusBadRequest, "release_id and strategy are required")
		return
	}
	principal, _ := auth.PrincipalFrom(r.Context())
	ro, err := a.updates.CreateRollout(r.Context(), req.ReleaseID, req.Strategy,
		req.NodeIDs, req.CanaryCount, req.BatchSize, principal.Name)
	if err != nil {
		switch {
		case errors.Is(err, updateplane.ErrReleaseNotApproved):
			writeErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, http.StatusNotFound, "release not found")
		case errors.Is(err, updateplane.ErrInvalidRollout):
			writeErr(w, http.StatusBadRequest, err.Error())
		default:
			a.internalErr(w, r, err)
		}
		return
	}
	a.st.Audit(r.Context(), principal.Name, "rollout.create", ro.ID,
		map[string]string{"release_id": req.ReleaseID, "strategy": req.Strategy})
	writeJSON(w, http.StatusCreated, ro)
}

func (a *API) handleApproveRollout(w http.ResponseWriter, r *http.Request) {
	ro, err := a.updates.ApproveNextBatch(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, updateplane.ErrNotAwaitingApproval) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "rollout not found")
			return
		}
		a.internalErr(w, r, err)
		return
	}
	principal, _ := auth.PrincipalFrom(r.Context())
	a.st.Audit(r.Context(), principal.Name, "rollout.approve", ro.ID, nil)
	writeJSON(w, http.StatusOK, ro)
}

func (a *API) handleCancelRollout(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.updates.CancelRollout(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, http.StatusNotFound, "rollout not found")
		case errors.Is(err, updateplane.ErrNotCancellable):
			writeErr(w, http.StatusConflict, err.Error())
		default:
			a.internalErr(w, r, err)
		}
		return
	}
	principal, _ := auth.PrincipalFrom(r.Context())
	a.st.Audit(r.Context(), principal.Name, "rollout.cancel", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

// ---- agent updates ----------------------------------------------------------

func (a *API) handleListAgentUpdates(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	ups, err := a.st.ListAgentUpdates(r.Context(),
		r.URL.Query().Get("rollout_id"),
		r.URL.Query().Get("node_id"),
		r.URL.Query().Get("phase"), limit)
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	if ups == nil {
		ups = []*store.AgentUpdate{}
	}
	writeJSON(w, http.StatusOK, ups)
}

func (a *API) handleGetAgentUpdate(w http.ResponseWriter, r *http.Request) {
	u, err := a.st.GetAgentUpdate(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "update not found")
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (a *API) handleCancelAgentUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.updates.CancelUpdate(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, http.StatusNotFound, "update not found")
		case errors.Is(err, updateplane.ErrNotCancellable):
			writeErr(w, http.StatusConflict, err.Error())
		default:
			a.internalErr(w, r, err)
		}
		return
	}
	principal, _ := auth.PrincipalFrom(r.Context())
	a.st.Audit(r.Context(), principal.Name, "agent_update.cancel", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

// ---- node-authenticated artifact proxy ---------------------------------------

// handleAgentReleaseFile serves a stored release file to an enrolled
// node. lmn_ bearer auth; the file must have been uploaded-and-verified
// (manifest digest) or registered verbatim (manifest/sums/signatures).
func (a *API) handleAgentReleaseFile(w http.ResponseWriter, r *http.Request) {
	if a.artifacts == nil {
		writeErr(w, http.StatusNotFound, "artifact store unavailable")
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !strings.HasPrefix(tok, creds.PrefixNode+"_") {
		writeErr(w, http.StatusUnauthorized, "node credential required")
		return
	}
	node, err := a.st.GetNodeByTokenHash(r.Context(), creds.Hash(tok))
	if err != nil || node.Status != contracts.NodeStatusApproved {
		writeErr(w, http.StatusUnauthorized, "invalid node credential")
		return
	}
	rel, err := a.updates.GetRelease(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "release not found")
		return
	}
	name := r.PathValue("name")
	if !releaseFileServable(rel, name) {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	f, size, err := a.artifacts.Open(rel, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeErr(w, http.StatusNotFound, "file not found")
			return
		}
		a.internalErr(w, r, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	http.ServeContent(w, r, name, rel.CreatedAt, f)
}

// releaseFileServable gates the proxy: only metadata files and artifacts
// declared by the release's signed manifest may leave the store. Keeps
// in-flight .upload-* temp files and anything else on disk unreachable.
func releaseFileServable(rel *store.Release, name string) bool {
	if name == release.ManifestFile || name == release.SumsFile {
		return true
	}
	if _, ok := rel.Signatures[name]; ok {
		return true
	}
	man, err := release.ParseManifest(rel.Manifest)
	if err != nil {
		return false
	}
	for _, a := range man.Artifacts {
		if a.File == name {
			return true
		}
	}
	return false
}
