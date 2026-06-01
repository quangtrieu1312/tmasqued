package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"github.com/quangtrieu1312/tmasqued/constants"
	"github.com/quangtrieu1312/tmasqued/logger"
	"github.com/quangtrieu1312/tmasqued/request"
	"github.com/quangtrieu1312/tmasqued/response"
	"github.com/quangtrieu1312/tmasqued/service"
)

// The management API is intentionally REST-shaped: a relationship is a
// sub-collection of the side that owns it, so the URL says what it does.
//
//	clients                         roles                           resources
//	GET    /clients                 GET    /roles                   GET    /resources
//	POST   /clients                 POST   /roles                   POST   /resources
//	GET    /clients/{id}            GET    /roles/{id}              GET    /resources/{id}
//	PATCH  /clients/{id}            PATCH  /roles/{id}              PATCH  /resources/{id}
//	DELETE /clients/{id}            DELETE /roles/{id}              DELETE /resources/{id}
//	GET    /clients/{id}/roles      POST   /roles/{id}/resources    (assign resources → role)
//	POST   /clients/{id}/roles      DELETE /roles/{id}/resources    (revoke resources ← role)
//	DELETE /clients/{id}/roles
//	GET    /clients/{id}/resources  (a client's effective routes)
//
//	dhcp:  GET /dhcp (pool state)   PUT /dhcp (reset pool)
//
// POST on a link sub-collection adds the listed ids; DELETE removes the listed
// ids (subset semantics — it never touches links you didn't name). Every create,
// link, unlink, and delete responds with {"ids":[…]} — the ids actually affected.

// --- small response helpers ------------------------------------------------

func dbg(format string, a ...any) {
	if logger.ShouldLog(logger.DEBUG) {
		logger.Debug(fmt.Sprintf(format, a...))
	}
}

// pathID parses the {id} path segment; writes 400 and returns ok=false on failure.
func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		if logger.ShouldLog(logger.TRACE) {
			logger.Trace(fmt.Sprintf("Invalid id %q: %v", r.PathValue("id"), err))
		}
		w.WriteHeader(http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

// decodeBody reads a JSON body into v; writes 400 and returns ok=false on failure.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		dbg("%s %s: invalid body: %v", r.Method, r.URL.Path, err)
		w.WriteHeader(http.StatusBadRequest)
		return false
	}
	return true
}

// writeJSON marshals v to the response; writes 500 on marshal failure.
func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		dbg("cannot marshal response: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Write(b)
}

// writeData fetches-and-marshals: 400 if the service errored, else the JSON body.
func writeData[T any](w http.ResponseWriter, data *T, err error, what string) {
	if err != nil {
		dbg("%s: %v", what, err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	writeJSON(w, *data)
}

// writeIDs responds with the affected ids for a create/link/unlink/delete:
// 400 if the service errored, else {"ids":[…]}.
func writeIDs(w http.ResponseWriter, ids *[]int64, err error, what string) {
	if err != nil {
		dbg("%s: %v", what, err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	writeJSON(w, response.IDs{IDs: *ids})
}

// writeOK turns a (bool, error) service result into 200 / 400 (used by renames + dhcp).
func writeOK(w http.ResponseWriter, ok bool, err error, what string) {
	if ok {
		w.WriteHeader(http.StatusOK)
		return
	}
	dbg("%s: %v", what, err)
	w.WriteHeader(http.StatusBadRequest)
}

func RunManagementService(ctx context.Context) {
	fd, err := net.Listen("unix", constants.MANAGEMENT_SOCKET_PATH)
	if err != nil {
		logger.Fatal(fmt.Sprintf("Cannot listen on unix socket %v: %v", constants.MANAGEMENT_SOCKET_PATH, err))
	}
	mux := http.NewServeMux()

	// ---- clients ----------------------------------------------------------
	mux.HandleFunc("GET /clients", func(w http.ResponseWriter, r *http.Request) {
		data, err := service.GetAllClients(ctx)
		writeData(w, data, err, "GET /clients")
	})
	mux.HandleFunc("POST /clients", func(w http.ResponseWriter, r *http.Request) {
		var body request.UpsertClients
		if !decodeBody(w, r, &body) {
			return
		}
		clientIDs, err := service.CreateClientsWithRoles(ctx, body.Names)
		if err != nil {
			dbg("POST /clients: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(w, response.IDs{IDs: *clientIDs})
	})
	mux.HandleFunc("GET /clients/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		data, err := service.GetClientByID(ctx, id)
		writeData(w, data, err, fmt.Sprintf("GET /clients/%d", id))
	})
	mux.HandleFunc("PATCH /clients/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var body request.UpdateName
		if !decodeBody(w, r, &body) {
			return
		}
		done, err := service.UpdateClientName(ctx, id, body.Name)
		writeOK(w, done, err, fmt.Sprintf("rename client %d", id))
	})
	mux.HandleFunc("DELETE /clients/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		ids, err := service.DeleteClients(ctx, []int64{id})
		writeIDs(w, ids, err, fmt.Sprintf("delete client %d", id))
	})
	mux.HandleFunc("GET /clients/{id}/roles", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		data, err := service.GetClientRoles(ctx, id)
		writeData(w, data, err, fmt.Sprintf("GET /clients/%d/roles", id))
	})
	mux.HandleFunc("POST /clients/{id}/roles", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var body request.RoleIDs
		if !decodeBody(w, r, &body) {
			return
		}
		ids, err := service.AssignRolesToClients(ctx, body.RoleIDs, []int64{id})
		writeIDs(w, ids, err, fmt.Sprintf("assign roles to client %d", id))
	})
	mux.HandleFunc("DELETE /clients/{id}/roles", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var body request.RoleIDs
		if !decodeBody(w, r, &body) {
			return
		}
		ids, err := service.UnassignRolesToClients(ctx, body.RoleIDs, []int64{id})
		writeIDs(w, ids, err, fmt.Sprintf("unassign roles from client %d", id))
	})
	mux.HandleFunc("GET /clients/{id}/resources", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		data, err := service.GetClientResources(ctx, id)
		writeData(w, data, err, fmt.Sprintf("GET /clients/%d/resources", id))
	})

	// ---- clients ↔ roles, addressed by name (server resolves names → ids) --
	mux.HandleFunc("POST /clients/by-name/{name}/roles", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var body request.RoleNames
		if !decodeBody(w, r, &body) {
			return
		}
		ids, err := service.AssignRolesToClientByName(ctx, name, body.RoleNames)
		writeIDs(w, ids, err, fmt.Sprintf("assign roles to client %q", name))
	})
	mux.HandleFunc("DELETE /clients/by-name/{name}/roles", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var body request.RoleNames
		if !decodeBody(w, r, &body) {
			return
		}
		ids, err := service.UnassignRolesToClientByName(ctx, name, body.RoleNames)
		writeIDs(w, ids, err, fmt.Sprintf("unassign roles from client %q", name))
	})

	// ---- roles ------------------------------------------------------------
	mux.HandleFunc("GET /roles", func(w http.ResponseWriter, r *http.Request) {
		data, err := service.GetAllRoles(ctx)
		writeData(w, data, err, "GET /roles")
	})
	mux.HandleFunc("POST /roles", func(w http.ResponseWriter, r *http.Request) {
		var body request.UpsertRoles
		if !decodeBody(w, r, &body) {
			return
		}
		ids, err := service.UpsertRoles(ctx, body.Names)
		writeIDs(w, ids, err, "POST /roles")
	})
	mux.HandleFunc("GET /roles/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		data, err := service.GetRoleByID(ctx, id)
		writeData(w, data, err, fmt.Sprintf("GET /roles/%d", id))
	})
	mux.HandleFunc("PATCH /roles/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var body request.UpdateName
		if !decodeBody(w, r, &body) {
			return
		}
		done, err := service.UpdateRoleName(ctx, id, body.Name)
		writeOK(w, done, err, fmt.Sprintf("rename role %d", id))
	})
	mux.HandleFunc("DELETE /roles/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		ids, err := service.DeleteRoles(ctx, []int64{id})
		writeIDs(w, ids, err, fmt.Sprintf("delete role %d", id))
	})
	mux.HandleFunc("POST /roles/{id}/resources", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var body request.ResourceIDs
		if !decodeBody(w, r, &body) {
			return
		}
		ids, err := service.AssignResourcesToRoles(ctx, body.ResourceIDs, []int64{id})
		writeIDs(w, ids, err, fmt.Sprintf("assign resources to role %d", id))
	})
	mux.HandleFunc("DELETE /roles/{id}/resources", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var body request.ResourceIDs
		if !decodeBody(w, r, &body) {
			return
		}
		ids, err := service.UnassignResourcesToRoles(ctx, body.ResourceIDs, []int64{id})
		writeIDs(w, ids, err, fmt.Sprintf("unassign resources from role %d", id))
	})

	// ---- roles ↔ resources, addressed by name (server resolves names → ids) --
	mux.HandleFunc("POST /roles/by-name/{name}/resources", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var body request.ResourceNames
		if !decodeBody(w, r, &body) {
			return
		}
		ids, err := service.AssignResourcesToRoleByName(ctx, name, body.ResourceNames)
		writeIDs(w, ids, err, fmt.Sprintf("assign resources to role %q", name))
	})
	mux.HandleFunc("DELETE /roles/by-name/{name}/resources", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var body request.ResourceNames
		if !decodeBody(w, r, &body) {
			return
		}
		ids, err := service.UnassignResourcesToRoleByName(ctx, name, body.ResourceNames)
		writeIDs(w, ids, err, fmt.Sprintf("unassign resources from role %q", name))
	})

	// ---- resources --------------------------------------------------------
	mux.HandleFunc("GET /resources", func(w http.ResponseWriter, r *http.Request) {
		data, err := service.GetAllResources(ctx)
		writeData(w, data, err, "GET /resources")
	})
	mux.HandleFunc("POST /resources", func(w http.ResponseWriter, r *http.Request) {
		var body request.UpsertResources
		if !decodeBody(w, r, &body) {
			return
		}
		ids, err := service.UpsertResources(ctx, body.Resources)
		writeIDs(w, ids, err, "POST /resources")
	})
	mux.HandleFunc("GET /resources/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		data, err := service.GetResourceByID(ctx, id)
		writeData(w, data, err, fmt.Sprintf("GET /resources/%d", id))
	})
	mux.HandleFunc("PATCH /resources/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var body request.UpdateName
		if !decodeBody(w, r, &body) {
			return
		}
		done, err := service.UpdateResourceName(ctx, id, body.Name)
		writeOK(w, done, err, fmt.Sprintf("rename resource %d", id))
	})
	mux.HandleFunc("DELETE /resources/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		ids, err := service.DeleteResources(ctx, []int64{id})
		writeIDs(w, ids, err, fmt.Sprintf("delete resource %d", id))
	})

	// ---- dhcp -------------------------------------------------------------
	mux.HandleFunc("GET /dhcp", func(w http.ResponseWriter, r *http.Request) {
		data, err := service.GetAllAvailableIPRanges(ctx)
		writeData(w, data, err, "GET /dhcp")
	})
	mux.HandleFunc("PUT /dhcp", func(w http.ResponseWriter, r *http.Request) {
		var body request.ResetDHCP
		if !decodeBody(w, r, &body) {
			return
		}
		done, err := service.ResetDHCP(ctx, body.FirstIP, body.LastIP)
		writeOK(w, done, err, "PUT /dhcp")
	})

	server := http.Server{
		Handler: mux,
	}
	go server.Serve(fd)
	defer server.Close()
	<-ctx.Done()
}
