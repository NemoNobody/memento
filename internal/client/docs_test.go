package client

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/NemoNobody/memento/internal/config"
	"github.com/NemoNobody/memento/internal/model"
)

func docsMutate(cfg *config.Config) {
	cfg.DocsIndexName = "memento-docs"
	cfg.FetchAllowPrivate = true // fetch targets are httptest loopback servers
}

func TestBuildDocsIndexBodyHTMLNotIndexed(t *testing.T) {
	props := buildDocsIndexBody()["mappings"].(map[string]interface{})["properties"].(map[string]interface{})
	htmlProp, ok := props["html"].(map[string]interface{})
	if !ok || htmlProp["index"] != false {
		t.Errorf("html mapping = %+v, want type text with index:false", props["html"])
	}
	for _, field := range []string{"sections", "images", "final_url", "content_hash", "size_raw"} {
		if _, ok := props[field]; !ok {
			t.Errorf("mapping lacks %q", field)
		}
	}
}

func TestBuildDocSearchBody(t *testing.T) {
	b := buildDocSearchBody("q", "", 0)
	if size, ok := b["size"].(int); !ok || size != 5 {
		t.Errorf("default size = %v, want 5", b["size"])
	}
	src := b["_source"].(map[string]interface{})
	ex, ok := src["excludes"].([]string)
	if !ok || len(ex) != 1 || ex[0] != "html" {
		t.Errorf("_source excludes = %v, want [html] — the snapshot must never ride along", src["excludes"])
	}
	must := b["query"].(map[string]interface{})["bool"].(map[string]interface{})["must"].(map[string]interface{})
	fields := must["multi_match"].(map[string]interface{})["fields"].([]string)
	if len(fields) != 2 || fields[0] != "title" || fields[1] != "sections.text" {
		t.Errorf("fields = %v, want [title sections.text]", fields)
	}

	capped := buildDocSearchBody("q", "proj", 100)
	if size := capped["size"].(int); size != 20 {
		t.Errorf("capped size = %d, want 20", size)
	}
	filters := capped["query"].(map[string]interface{})["bool"].(map[string]interface{})["filter"].([]map[string]interface{})
	if len(filters) != 1 {
		t.Fatalf("filters = %d, want 1 (project)", len(filters))
	}
	term := filters[0]["term"].(map[string]interface{})
	if term["project"] != "proj" {
		t.Errorf("project filter = %v", term)
	}
}

func TestGetDocByHashExcludesHTML(t *testing.T) {
	c, fake := newFakeClientCfg(t, docsMutate, func(r recordedRequest) (int, string) {
		if r.Path == "/memento-docs/_doc/h1" && r.Method == "GET" {
			return 200, `{"found":true,"_source":{"url":"https://x","url_hash":"h1","title":"T","sections":[{"order":0,"heading":"A","text":"a"}]}}`
		}
		return 404, "{}"
	})
	doc, err := c.GetDocByHash(context.Background(), "h1")
	if err != nil {
		t.Fatalf("GetDocByHash: %v", err)
	}
	if doc.Title != "T" || len(doc.Sections) != 1 {
		t.Errorf("doc = %+v", doc)
	}
	reqs := fake.requestsFor("GET", "/memento-docs/_doc/h1")
	if len(reqs) != 1 {
		t.Fatalf("GET requests = %d, want 1", len(reqs))
	}
	if got := reqs[0].Query.Get("_source_excludes"); got != "html" {
		t.Errorf("_source_excludes = %q, want html", got)
	}
}

func TestPutDocRefreshesAndWait(t *testing.T) {
	c, fake := newFakeClientCfg(t, docsMutate, func(r recordedRequest) (int, string) {
		return 200, `{"result":"created"}`
	})
	doc := &model.Doc{URLHash: "h1", Sections: []model.Section{{Order: 0, Text: "a"}}, HTML: "<h1>x</h1>"}
	if err := c.PutDoc(context.Background(), doc); err != nil {
		t.Fatalf("PutDoc: %v", err)
	}
	var rec *recordedRequest
	for i, r := range fake.requests {
		if strings.HasPrefix(r.Path, "/memento-docs/_doc/h1") {
			rec = &fake.requests[i]
			break
		}
	}
	if rec == nil {
		t.Fatalf("no write to /memento-docs/_doc/h1; requests: %+v", fake.requests)
	}
	if rec.Method != "PUT" {
		t.Errorf("method = %s, want PUT", rec.Method)
	}
	if rec.Query.Get("refresh") != "wait_for" {
		t.Errorf("refresh = %q, want wait_for (read-after-write)", rec.Query.Get("refresh"))
	}
	if !strings.Contains(rec.Body, `"html"`) || !strings.Contains(rec.Body, "sections") {
		t.Errorf("stored body lacks html/sections: %s", rec.Body)
	}
}

func TestSearchDocsContract(t *testing.T) {
	c, fake := newFakeClientCfg(t, docsMutate, func(r recordedRequest) (int, string) {
		return 200, `{"hits":{"hits":[]}}`
	})
	docs, _, err := c.SearchDocs(context.Background(), "q", "proj", 3)
	if err != nil {
		t.Fatalf("SearchDocs: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("docs = %d, want 0", len(docs))
	}
	reqs := fake.requestsFor("POST", "/memento-docs/_search")
	if len(reqs) != 1 {
		t.Fatalf("search requests = %d, want 1", len(reqs))
	}
	for _, want := range []string{`"size":3`, `"excludes":["html"]`, `"project":"proj"`} {
		if !strings.Contains(reqs[0].Body, want) {
			t.Errorf("body lacks %s: %s", want, reqs[0].Body)
		}
	}
}

func TestFetchDocStoresSnapshotThenServesCache(t *testing.T) {
	ft := newFetchTarget(t)
	var (
		indexExists bool
		stored      string
	)
	respond := func(r recordedRequest) (int, string) {
		switch {
		case r.Path == "/memento-docs" && r.Method == "HEAD":
			if indexExists {
				return 200, ""
			}
			return 404, ""
		case r.Path == "/memento-docs" && r.Method == "PUT":
			indexExists = true
			return 200, `{"acknowledged":true}`
		case strings.HasPrefix(r.Path, "/memento-docs/_doc/"):
			if r.Method == "GET" {
				if stored == "" {
					return 404, "{}"
				}
				return 200, `{"found":true,"_source":` + stored + `}`
			}
			stored = r.Body
			return 200, `{"result":"created"}`
		}
		return 404, "{}"
	}
	c, fake := newFakeClientCfg(t, docsMutate, respond)
	ctx := context.Background()
	url := ft.srv.URL + "/page"

	doc1, cached, err := c.FetchDoc(ctx, url, "proj", false, nil)
	if err != nil {
		t.Fatalf("FetchDoc(first): %v", err)
	}
	if cached {
		t.Error("first fetch must not be cached")
	}
	if stored == "" {
		t.Fatal("nothing was stored")
	}
	var storedDoc model.Doc
	if err := json.Unmarshal([]byte(stored), &storedDoc); err != nil {
		t.Fatalf("stored doc not decodable: %v", err)
	}
	if storedDoc.HTML == "" {
		t.Error("the snapshot html must be stored (re-extraction, shared cache)")
	}
	if storedDoc.Project != "proj" || storedDoc.ContentHash == "" || storedDoc.Via != ViaDirect {
		t.Errorf("stored audit fields: %+v", storedDoc)
	}

	doc2, cached2, err := c.FetchDoc(ctx, url, "proj", false, nil)
	if err != nil {
		t.Fatalf("FetchDoc(second): %v", err)
	}
	if !cached2 {
		t.Error("second fetch must hit the cache")
	}
	if doc2.Title != doc1.Title {
		t.Errorf("cached title = %q, want %q", doc2.Title, doc1.Title)
	}
	puts := 0
	for _, r := range fake.requests {
		if r.Method == "PUT" && strings.HasPrefix(r.Path, "/memento-docs/_doc/") {
			puts++
		}
	}
	if puts != 1 {
		t.Errorf("writes = %d, want 1: a cache hit must not rewrite", puts)
	}
}
