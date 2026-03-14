package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/testutil"
)

func TestCatalogAdminSeesAllApps(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	srv.DB.UpsertUserWithRole("publisher-1", "pub1@example.com", "Publisher 1", "publisher")
	srv.DB.UpsertUserWithRole("publisher-2", "pub2@example.com", "Publisher 2", "publisher")

	// Two different publishers create apps
	pub1Token := idp.IssueJWT("publisher-1", []string{})
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", pub1Token,
			strings.NewReader(`{"name":"app-one"}`)))
	resp.Body.Close()

	pub2Token := idp.IssueJWT("publisher-2", []string{})
	resp, _ = http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", pub2Token,
			strings.NewReader(`{"name":"app-two"}`)))
	resp.Body.Close()

	// Admin queries catalog — should see both
	adminToken := idp.IssueJWT("admin-1", []string{})
	resp, err := http.DefaultClient.Do(
		jwtReq("GET", ts.URL+"/api/v1/catalog", adminToken, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	total := int(result["total"].(float64))
	if total != 2 {
		t.Errorf("expected total=2, got %d", total)
	}
	items := result["items"].([]interface{})
	if len(items) != 2 {
		t.Errorf("expected 2 items, got %d", len(items))
	}
}

func TestCatalogTagFilter(t *testing.T) {
	_, ts := testServer(t)

	// Create two apps
	app1 := createApp(t, ts, "tagged-app")
	app1ID := app1["id"].(string)
	createApp(t, ts, "untagged-app")

	// Create a tag and assign it to app1 only
	req := authReq("POST", ts.URL+"/api/v1/tags", strings.NewReader(`{"name":"special"}`))
	resp, _ := http.DefaultClient.Do(req)
	var tag map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&tag)
	resp.Body.Close()
	tagID := tag["id"].(string)

	req = authReq("POST", ts.URL+"/api/v1/apps/"+app1ID+"/tags",
		strings.NewReader(`{"tag_id":"`+tagID+`"}`))
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()

	// Filter catalog by tag name
	req = authReq("GET", ts.URL+"/api/v1/catalog?tag=special", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	total := int(result["total"].(float64))
	if total != 1 {
		t.Errorf("expected total=1, got %d", total)
	}
	items := result["items"].([]interface{})
	if len(items) != 1 {
		t.Errorf("expected 1 item, got %d", len(items))
	}
	item := items[0].(map[string]interface{})
	if item["name"] != "tagged-app" {
		t.Errorf("expected tagged-app, got %v", item["name"])
	}
}

func TestCatalogSearchFilter(t *testing.T) {
	srv, ts := testServer(t)

	// Create apps with different names
	app1 := createApp(t, ts, "analytics-dashboard")
	createApp(t, ts, "data-pipeline")

	// Set title and description on app1 to test search coverage
	title := "Revenue Analytics"
	desc := "Tracks revenue metrics"
	srv.DB.UpdateApp(app1["id"].(string), db.AppUpdate{Title: &title, Description: &desc})

	// Search by name substring
	req := authReq("GET", ts.URL+"/api/v1/catalog?search=analytics", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	total := int(result["total"].(float64))
	if total != 1 {
		t.Errorf("search by name: expected total=1, got %d", total)
	}

	// Search by title
	req = authReq("GET", ts.URL+"/api/v1/catalog?search=revenue", nil)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	var result2 map[string]interface{}
	json.NewDecoder(resp2.Body).Decode(&result2)
	total2 := int(result2["total"].(float64))
	if total2 != 1 {
		t.Errorf("search by title: expected total=1, got %d", total2)
	}

	// Search by description
	req = authReq("GET", ts.URL+"/api/v1/catalog?search=metrics", nil)
	resp3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()

	var result3 map[string]interface{}
	json.NewDecoder(resp3.Body).Decode(&result3)
	total3 := int(result3["total"].(float64))
	if total3 != 1 {
		t.Errorf("search by description: expected total=1, got %d", total3)
	}
}

func TestCatalogPagination(t *testing.T) {
	_, ts := testServer(t)

	// Create 5 apps
	for _, name := range []string{"app-a", "app-b", "app-c", "app-d", "app-e"} {
		createApp(t, ts, name)
	}

	// Request page 1 with per_page=2
	req := authReq("GET", ts.URL+"/api/v1/catalog?page=1&per_page=2", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	total := int(result["total"].(float64))
	if total != 5 {
		t.Errorf("expected total=5, got %d", total)
	}
	page := int(result["page"].(float64))
	if page != 1 {
		t.Errorf("expected page=1, got %d", page)
	}
	perPage := int(result["per_page"].(float64))
	if perPage != 2 {
		t.Errorf("expected per_page=2, got %d", perPage)
	}
	items := result["items"].([]interface{})
	if len(items) != 2 {
		t.Errorf("expected 2 items on page 1, got %d", len(items))
	}

	// Request page 3 — should have 1 item
	req = authReq("GET", ts.URL+"/api/v1/catalog?page=3&per_page=2", nil)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	var result2 map[string]interface{}
	json.NewDecoder(resp2.Body).Decode(&result2)
	items2 := result2["items"].([]interface{})
	if len(items2) != 1 {
		t.Errorf("expected 1 item on page 3, got %d", len(items2))
	}
}

func TestCatalogEmpty(t *testing.T) {
	_, ts := testServer(t)

	req := authReq("GET", ts.URL+"/api/v1/catalog", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	total := int(result["total"].(float64))
	if total != 0 {
		t.Errorf("expected total=0, got %d", total)
	}
	items := result["items"].([]interface{})
	if len(items) != 0 {
		t.Errorf("expected 0 items, got %d", len(items))
	}
}
