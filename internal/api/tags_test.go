package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/testutil"
)

func TestCreateTag(t *testing.T) {
	_, ts := testServer(t)

	body := `{"name":"my-tag"}`
	req := authReq("POST", ts.URL+"/api/v1/tags", strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, b)
	}

	var tag map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&tag)
	if tag["id"] == nil || tag["id"] == "" {
		t.Error("expected non-empty id")
	}
	if tag["name"] != "my-tag" {
		t.Errorf("expected name=my-tag, got %v", tag["name"])
	}
	if tag["created_at"] == nil || tag["created_at"] == "" {
		t.Error("expected non-empty created_at")
	}
}

func TestCreateDuplicateTag(t *testing.T) {
	_, ts := testServer(t)

	body := `{"name":"dup-tag"}`
	req := authReq("POST", ts.URL+"/api/v1/tags", strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	req = authReq("POST", ts.URL+"/api/v1/tags", strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409, got %d", resp.StatusCode)
	}
}

func TestListTags(t *testing.T) {
	_, ts := testServer(t)

	// Create tags in non-alphabetical order
	for _, name := range []string{"zeta", "alpha", "mid"} {
		req := authReq("POST", ts.URL+"/api/v1/tags", strings.NewReader(`{"name":"`+name+`"}`))
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
	}

	req := authReq("GET", ts.URL+"/api/v1/tags", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var tags []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&tags)
	if len(tags) != 3 {
		t.Fatalf("expected 3 tags, got %d", len(tags))
	}

	// Should be sorted by name
	expected := []string{"alpha", "mid", "zeta"}
	for i, name := range expected {
		if tags[i]["name"] != name {
			t.Errorf("tag[%d]: expected %q, got %v", i, name, tags[i]["name"])
		}
	}
}

func TestDeleteTag(t *testing.T) {
	_, ts := testServer(t)

	// Create a tag
	req := authReq("POST", ts.URL+"/api/v1/tags", strings.NewReader(`{"name":"doomed"}`))
	resp, _ := http.DefaultClient.Do(req)
	var tag map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&tag)
	resp.Body.Close()
	tagID := tag["id"].(string)

	// Delete it
	req = authReq("DELETE", ts.URL+"/api/v1/tags/"+tagID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("expected 204, got %d", resp.StatusCode)
	}
}

func TestDeleteNonexistentTag(t *testing.T) {
	_, ts := testServer(t)

	req := authReq("DELETE", ts.URL+"/api/v1/tags/nonexistent-id", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestAddTagToApp(t *testing.T) {
	_, ts := testServer(t)

	// Create an app
	app := createApp(t, ts, "tagged-app")
	appID := app["id"].(string)

	// Create a tag
	req := authReq("POST", ts.URL+"/api/v1/tags", strings.NewReader(`{"name":"important"}`))
	resp, _ := http.DefaultClient.Do(req)
	var tag map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&tag)
	resp.Body.Close()
	tagID := tag["id"].(string)

	// Add tag to app
	req = authReq("POST", ts.URL+"/api/v1/apps/"+appID+"/tags",
		strings.NewReader(`{"tag_id":"`+tagID+`"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 204, got %d: %s", resp.StatusCode, b)
	}
}

func TestRemoveTagFromApp(t *testing.T) {
	_, ts := testServer(t)

	// Create app and tag
	app := createApp(t, ts, "tagged-app")
	appID := app["id"].(string)

	req := authReq("POST", ts.URL+"/api/v1/tags", strings.NewReader(`{"name":"removable"}`))
	resp, _ := http.DefaultClient.Do(req)
	var tag map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&tag)
	resp.Body.Close()
	tagID := tag["id"].(string)

	// Add tag to app
	req = authReq("POST", ts.URL+"/api/v1/apps/"+appID+"/tags",
		strings.NewReader(`{"tag_id":"`+tagID+`"}`))
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()

	// Remove tag from app
	req = authReq("DELETE", ts.URL+"/api/v1/apps/"+appID+"/tags/"+tagID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("expected 204, got %d", resp.StatusCode)
	}
}

func TestNonAdminCannotCreateTags(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	// Publisher role cannot manage tags (only admin can)
	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "publisher")
	token := createTestPAT(t, srv.DB, "user-1")

	resp, err := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/tags", token,
			strings.NewReader(`{"name":"denied-tag"}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for non-admin, got %d", resp.StatusCode)
	}
}

func TestCreateTagInvalidJSON(t *testing.T) {
	_, ts := testServer(t)

	req := authReq("POST", ts.URL+"/api/v1/tags", strings.NewReader(`{not json`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestCreateTagEmptyName(t *testing.T) {
	_, ts := testServer(t)

	req := authReq("POST", ts.URL+"/api/v1/tags", strings.NewReader(`{"name":""}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestCreateTagInvalidName(t *testing.T) {
	_, ts := testServer(t)

	req := authReq("POST", ts.URL+"/api/v1/tags", strings.NewReader(`{"name":"UPPER"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestAddTagToAppMissingTagID(t *testing.T) {
	_, ts := testServer(t)

	app := createApp(t, ts, "tag-app")
	appID := app["id"].(string)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+appID+"/tags",
		strings.NewReader(`{"tag_id":""}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestAddTagToAppNonexistentTag(t *testing.T) {
	_, ts := testServer(t)

	app := createApp(t, ts, "tag-app")
	appID := app["id"].(string)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+appID+"/tags",
		strings.NewReader(`{"tag_id":"nonexistent-tag-id"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestAddTagToAppInvalidJSON(t *testing.T) {
	_, ts := testServer(t)

	app := createApp(t, ts, "tag-app")
	appID := app["id"].(string)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+appID+"/tags",
		strings.NewReader(`{not json`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestRemoveTagFromAppNonexistent(t *testing.T) {
	_, ts := testServer(t)

	app := createApp(t, ts, "tag-app")
	appID := app["id"].(string)

	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+appID+"/tags/nonexistent-tag-id", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestListTagsEmpty(t *testing.T) {
	_, ts := testServer(t)

	req := authReq("GET", ts.URL+"/api/v1/tags", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var tags []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&tags)
	if len(tags) != 0 {
		t.Errorf("expected 0 tags, got %d", len(tags))
	}
}

func TestCreateTagNameEdgeCases(t *testing.T) {
	_, ts := testServer(t)

	cases := []struct {
		name       string
		wantStatus int
	}{
		{"1starts-with-digit", http.StatusBadRequest},
		{"ends-with-hyphen-", http.StatusBadRequest},
		{"has_underscore", http.StatusBadRequest},
		{"HAS-UPPER", http.StatusBadRequest},
		{"a", http.StatusCreated},        // single letter is valid
		{"a-b-c", http.StatusCreated},    // hyphens in the middle
		{"abc123", http.StatusCreated},   // letters and digits
	}

	for _, tc := range cases {
		req := authReq("POST", ts.URL+"/api/v1/tags",
			strings.NewReader(`{"name":"`+tc.name+`"}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		if resp.StatusCode != tc.wantStatus {
			t.Errorf("name %q: expected %d, got %d", tc.name, tc.wantStatus, resp.StatusCode)
		}
	}
}
