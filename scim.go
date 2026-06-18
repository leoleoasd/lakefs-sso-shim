package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Minimal SCIM 2.0 server that maps IdP-pushed user/group lifecycle onto the
// lakeFS ACL server. Its main purpose is real-time deprovisioning: when the IdP
// deletes/disables a user, lakeFS removes them (and thus their access).
//
// Resource ids are the natural lakeFS keys: User.id == userName, Group.id == displayName.

var scimToken = os.Getenv("SCIM_TOKEN")

const (
	scimUserSchema  = "urn:ietf:params:scim:schemas:core:2.0:User"
	scimGroupSchema = "urn:ietf:params:scim:schemas:core:2.0:Group"
	scimListSchema  = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	scimErrSchema   = "urn:ietf:params:scim:api:messages:2.0:Error"
)

var filterRe = regexp.MustCompile(`(?i)(\w+)\s+eq\s+"([^"]+)"`)

func scimEnabled() bool { return scimToken != "" }

func registerSCIM(mux *http.ServeMux) {
	mux.HandleFunc("/scim/v2/ServiceProviderConfig", scimAuth(scimServiceProviderConfig))
	mux.HandleFunc("/scim/v2/ResourceTypes", scimAuth(scimResourceTypes))
	mux.HandleFunc("/scim/v2/Schemas", scimAuth(func(w http.ResponseWriter, r *http.Request) { scimList(w, nil, 0) }))
	mux.HandleFunc("/scim/v2/Users", scimAuth(scimUsers))
	mux.HandleFunc("/scim/v2/Users/", scimAuth(scimUserByID))
	mux.HandleFunc("/scim/v2/Groups", scimAuth(scimGroups))
	mux.HandleFunc("/scim/v2/Groups/", scimAuth(scimGroupByID))
	log.Printf("SCIM enabled at /scim/v2")
}

func scimAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want := "Bearer " + scimToken
		if scimToken == "" || r.Header.Get("Authorization") != want {
			scimError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		h(w, r)
	}
}

func scimJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/scim+json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func scimError(w http.ResponseWriter, code int, detail string) {
	scimJSON(w, code, map[string]interface{}{
		"schemas": []string{scimErrSchema}, "detail": detail, "status": fmt.Sprint(code),
	})
}

func parseFilter(q string) (attr, val string) {
	if m := filterRe.FindStringSubmatch(q); m != nil {
		return strings.ToLower(m[1]), m[2]
	}
	return "", ""
}

func userResource(username string) map[string]interface{} {
	return map[string]interface{}{
		"schemas":  []string{scimUserSchema},
		"id":       username,
		"userName": username,
		"active":   true,
		"meta":     map[string]interface{}{"resourceType": "User", "location": "/scim/v2/Users/" + username},
	}
}

func groupResource(name string, members []string) map[string]interface{} {
	ms := []map[string]interface{}{}
	for _, m := range members {
		ms = append(ms, map[string]interface{}{"value": m, "display": m})
	}
	return map[string]interface{}{
		"schemas":     []string{scimGroupSchema},
		"id":          name,
		"displayName": name,
		"members":     ms,
		"meta":        map[string]interface{}{"resourceType": "Group", "location": "/scim/v2/Groups/" + name},
	}
}

func scimList(w http.ResponseWriter, resources []map[string]interface{}, total int) {
	if resources == nil {
		resources = []map[string]interface{}{}
	}
	scimJSON(w, http.StatusOK, map[string]interface{}{
		"schemas":      []string{scimListSchema},
		"totalResults": total,
		"startIndex":   1,
		"itemsPerPage": len(resources),
		"Resources":    resources,
	})
}

// ---------------- Users ----------------

func scimUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		attr, val := parseFilter(r.URL.Query().Get("filter"))
		if attr == "username" {
			if aclUserExists(val) {
				scimList(w, []map[string]interface{}{userResource(val)}, 1)
			} else {
				scimList(w, nil, 0)
			}
			return
		}
		// full list
		users, _ := aclListUsers()
		res := make([]map[string]interface{}, 0, len(users))
		for _, u := range users {
			res = append(res, userResource(u))
		}
		scimList(w, res, len(res))
	case http.MethodPost:
		var body struct {
			UserName string `json:"userName"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.UserName == "" {
			scimError(w, http.StatusBadRequest, "userName required")
			return
		}
		code, err := aclReq("POST", "/users", mustJSON(map[string]string{"username": body.UserName}), 201, 409)
		_ = code
		if err != nil {
			scimError(w, http.StatusBadGateway, err.Error())
			return
		}
		scimJSON(w, http.StatusCreated, userResource(body.UserName))
	default:
		scimError(w, http.StatusMethodNotAllowed, r.Method)
	}
}

func scimUserByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/scim/v2/Users/")
	id, _ = url.PathUnescape(id)
	switch r.Method {
	case http.MethodGet:
		if aclUserExists(id) {
			scimJSON(w, http.StatusOK, userResource(id))
		} else {
			scimError(w, http.StatusNotFound, "user not found")
		}
	case http.MethodPut:
		// upsert
		_, _ = aclReq("POST", "/users", mustJSON(map[string]string{"username": id}), 201, 409)
		scimJSON(w, http.StatusOK, userResource(id))
	case http.MethodPatch:
		// deactivation => revoke by deleting the lakeFS user
		if scimPatchHasActiveFalse(r) {
			_, _ = aclReq("DELETE", "/users/"+url.PathEscape(id), nil, 204, 404)
			log.Printf("SCIM: deactivated -> deleted lakeFS user %s", id)
			scimJSON(w, http.StatusOK, map[string]interface{}{"schemas": []string{scimUserSchema}, "id": id, "userName": id, "active": false})
			return
		}
		scimJSON(w, http.StatusOK, userResource(id))
	case http.MethodDelete:
		if _, err := aclReq("DELETE", "/users/"+url.PathEscape(id), nil, 204, 404); err != nil {
			scimError(w, http.StatusBadGateway, err.Error())
			return
		}
		log.Printf("SCIM: deleted lakeFS user %s", id)
		w.WriteHeader(http.StatusNoContent)
	default:
		scimError(w, http.StatusMethodNotAllowed, r.Method)
	}
}

// ---------------- Groups ----------------

func scimGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		attr, val := parseFilter(r.URL.Query().Get("filter"))
		groups, _ := aclListGroups()
		if attr == "displayname" {
			if contains(groups, val) {
				scimJSON(w, http.StatusOK, map[string]interface{}{
					"schemas": []string{scimListSchema}, "totalResults": 1, "startIndex": 1, "itemsPerPage": 1,
					"Resources": []map[string]interface{}{groupResource(val, aclGroupMembers(val))},
				})
			} else {
				scimList(w, nil, 0)
			}
			return
		}
		res := make([]map[string]interface{}, 0, len(groups))
		for _, g := range groups {
			res = append(res, groupResource(g, aclGroupMembers(g)))
		}
		scimList(w, res, len(res))
	case http.MethodPost:
		var body struct {
			DisplayName string `json:"displayName"`
			Members     []struct {
				Value string `json:"value"`
			} `json:"members"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.DisplayName == "" {
			scimError(w, http.StatusBadRequest, "displayName required")
			return
		}
		_, err := aclReq("POST", "/groups", mustJSON(map[string]string{"id": body.DisplayName}), 201, 409)
		if err != nil {
			scimError(w, http.StatusBadGateway, err.Error())
			return
		}
		for _, m := range body.Members {
			_, _ = aclReq("PUT", "/groups/"+url.PathEscape(body.DisplayName)+"/members/"+url.PathEscape(m.Value), nil, 201, 409)
		}
		scimJSON(w, http.StatusCreated, groupResource(body.DisplayName, nil))
	default:
		scimError(w, http.StatusMethodNotAllowed, r.Method)
	}
}

func scimGroupByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/scim/v2/Groups/")
	id, _ = url.PathUnescape(id)
	switch r.Method {
	case http.MethodGet:
		if contains(mustGroups(), id) {
			scimJSON(w, http.StatusOK, groupResource(id, aclGroupMembers(id)))
		} else {
			scimError(w, http.StatusNotFound, "group not found")
		}
	case http.MethodPatch:
		scimGroupPatch(w, r, id)
	case http.MethodPut:
		var body struct {
			Members []struct {
				Value string `json:"value"`
			} `json:"members"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		want := map[string]bool{}
		for _, m := range body.Members {
			want[m.Value] = true
		}
		// reconcile: add missing, remove extra
		cur := map[string]bool{}
		for _, m := range aclGroupMembers(id) {
			cur[m] = true
		}
		for u := range want {
			if !cur[u] {
				_, _ = aclReq("PUT", "/groups/"+url.PathEscape(id)+"/members/"+url.PathEscape(u), nil, 201, 409)
			}
		}
		for u := range cur {
			if !want[u] {
				_, _ = aclReq("DELETE", "/groups/"+url.PathEscape(id)+"/members/"+url.PathEscape(u), nil, 204, 404)
			}
		}
		scimJSON(w, http.StatusOK, groupResource(id, aclGroupMembers(id)))
	case http.MethodDelete:
		if _, err := aclReq("DELETE", "/groups/"+url.PathEscape(id), nil, 204, 404); err != nil {
			scimError(w, http.StatusBadGateway, err.Error())
			return
		}
		log.Printf("SCIM: deleted lakeFS group %s", id)
		w.WriteHeader(http.StatusNoContent)
	default:
		scimError(w, http.StatusMethodNotAllowed, r.Method)
	}
}

// SCIM PATCH for group membership (op add/remove/replace on "members")
func scimGroupPatch(w http.ResponseWriter, r *http.Request, group string) {
	var body struct {
		Operations []struct {
			Op    string          `json:"op"`
			Path  string          `json:"path"`
			Value json.RawMessage `json:"value"`
		} `json:"Operations"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		scimError(w, http.StatusBadRequest, "bad patch body")
		return
	}
	memberValRe := regexp.MustCompile(`value\s+eq\s+"([^"]+)"`)
	for _, op := range body.Operations {
		switch strings.ToLower(op.Op) {
		case "add":
			for _, u := range extractMemberValues(op.Value) {
				_, _ = aclReq("PUT", "/groups/"+url.PathEscape(group)+"/members/"+url.PathEscape(u), nil, 201, 409)
				log.Printf("SCIM: add %s to %s", u, group)
			}
		case "remove":
			// remove can target a specific member via path filter members[value eq "x"]
			if m := memberValRe.FindStringSubmatch(op.Path); m != nil {
				_, _ = aclReq("DELETE", "/groups/"+url.PathEscape(group)+"/members/"+url.PathEscape(m[1]), nil, 204, 404)
				log.Printf("SCIM: remove %s from %s", m[1], group)
			} else {
				for _, u := range extractMemberValues(op.Value) {
					_, _ = aclReq("DELETE", "/groups/"+url.PathEscape(group)+"/members/"+url.PathEscape(u), nil, 204, 404)
					log.Printf("SCIM: remove %s from %s", u, group)
				}
			}
		case "replace":
			// replace whole member set
			vals := extractMemberValues(op.Value)
			want := map[string]bool{}
			for _, u := range vals {
				want[u] = true
			}
			cur := aclGroupMembers(group)
			for u := range want {
				_, _ = aclReq("PUT", "/groups/"+url.PathEscape(group)+"/members/"+url.PathEscape(u), nil, 201, 409)
			}
			for _, u := range cur {
				if !want[u] {
					_, _ = aclReq("DELETE", "/groups/"+url.PathEscape(group)+"/members/"+url.PathEscape(u), nil, 204, 404)
				}
			}
		}
	}
	scimJSON(w, http.StatusOK, groupResource(group, aclGroupMembers(group)))
}

// value may be [{"value":"u"}] or {"members":[{"value":"u"}]} or a raw string
func extractMemberValues(raw json.RawMessage) []string {
	var arr []struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(raw, &arr) == nil && len(arr) > 0 {
		out := []string{}
		for _, a := range arr {
			if a.Value != "" {
				out = append(out, a.Value)
			}
		}
		return out
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return []string{s}
	}
	return nil
}

func scimPatchHasActiveFalse(r *http.Request) bool {
	var body struct {
		Operations []struct {
			Op    string          `json:"op"`
			Path  string          `json:"path"`
			Value json.RawMessage `json:"value"`
		} `json:"Operations"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		return false
	}
	for _, op := range body.Operations {
		if strings.EqualFold(op.Path, "active") {
			var b bool
			if json.Unmarshal(op.Value, &b) == nil && !b {
				return true
			}
			var s string
			if json.Unmarshal(op.Value, &s) == nil && strings.EqualFold(s, "false") {
				return true
			}
		}
	}
	return false
}

// ---------------- discovery stubs ----------------

func scimServiceProviderConfig(w http.ResponseWriter, r *http.Request) {
	scimJSON(w, http.StatusOK, map[string]interface{}{
		"schemas":          []string{"urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"},
		"patch":            map[string]bool{"supported": true},
		"bulk":             map[string]interface{}{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":           map[string]interface{}{"supported": true, "maxResults": 1000},
		"changePassword":   map[string]bool{"supported": false},
		"sort":             map[string]bool{"supported": false},
		"etag":             map[string]bool{"supported": false},
		"authenticationSchemes": []map[string]interface{}{
			{"type": "oauthbearertoken", "name": "OAuth Bearer Token", "primary": true},
		},
	})
}

func scimResourceTypes(w http.ResponseWriter, r *http.Request) {
	scimJSON(w, http.StatusOK, map[string]interface{}{
		"schemas":      []string{scimListSchema},
		"totalResults": 2, "startIndex": 1, "itemsPerPage": 2,
		"Resources": []map[string]interface{}{
			{"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:ResourceType"}, "id": "User", "name": "User", "endpoint": "/Users", "schema": scimUserSchema},
			{"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:ResourceType"}, "id": "Group", "name": "Group", "endpoint": "/Groups", "schema": scimGroupSchema},
		},
	})
}

// ---------------- ACL server helpers (SCIM-specific) ----------------

func aclUserExists(username string) bool {
	_, err := aclReq("GET", "/users/"+url.PathEscape(username), nil, 200)
	return err == nil
}

func aclListUsers() ([]string, error) {
	b, err := aclReq("GET", "/users?prefix=&after=&amount=1000", nil, 200)
	if err != nil {
		return nil, err
	}
	var out struct {
		Results []struct {
			Username string `json:"username"`
		} `json:"results"`
	}
	_ = json.Unmarshal(b, &out)
	res := []string{}
	for _, u := range out.Results {
		res = append(res, u.Username)
	}
	return res, nil
}

func aclListGroups() ([]string, error) {
	m, err := listLakeFSGroups()
	if err != nil {
		return nil, err
	}
	res := []string{}
	for g := range m {
		res = append(res, g)
	}
	return res, nil
}

func mustGroups() []string { g, _ := aclListGroups(); return g }

func aclGroupMembers(group string) []string {
	b, err := aclReq("GET", "/groups/"+url.PathEscape(group)+"/members?prefix=&after=&amount=1000", nil, 200)
	if err != nil {
		return nil
	}
	var out struct {
		Results []struct {
			Username string `json:"username"`
		} `json:"results"`
	}
	_ = json.Unmarshal(b, &out)
	res := []string{}
	for _, u := range out.Results {
		res = append(res, u.Username)
	}
	return res
}

func mustJSON(v interface{}) []byte { b, _ := json.Marshal(v); return b }

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
