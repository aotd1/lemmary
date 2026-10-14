package appapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
	"github.com/pocketbase/pocketbase/tools/security"
)

func mcpFilesServer(t *testing.T, app core.App) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+mcpFileRoute, func(w http.ResponseWriter, r *http.Request) {
		e := &core.RequestEvent{}
		e.App, e.Response, e.Request = app, w, r
		if err := handleMCPFile(app)(e); err != nil {
			t.Errorf("serve file: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestMCPFileLinksReturnOriginalAndPreviewWithoutText(t *testing.T) {
	app := bootQueueApp(t)
	owner := makeQueueUser(t, app, "files@example.com")
	doc := makeQueueDocument(t, app, owner, "completed", "OCR text")
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a5Z0AAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	file, err := filesystem.NewFileFromBytes(png, "first-page.png")
	if err != nil {
		t.Fatal(err)
	}
	doc.Set("preview", file)
	if err := app.Save(doc); err != nil {
		t.Fatal(err)
	}
	srv := mcpFilesServer(t, app)
	app.Settings().Meta.AppURL = srv.URL
	docs := withMCPFileLinks(app, owner, newMCPDocs(app, owner))
	session := connectMCP(t, agentTools{}, docs)
	var out mcpGetResult
	res := callMCP(t, session, "get_document", map[string]any{"id": doc.Id, "include_text": false}, &out)
	if res.IsError || out.Text != "" || out.TextChars != 8 || out.Truncated {
		t.Fatalf("metadata response: %#v, error=%v", out, res.IsError)
	}
	if out.OriginalFile == nil || out.Preview == nil {
		t.Fatal("missing file links")
	}
	for _, part := range []struct {
		link  *mcpFileLink
		bytes []byte
		mime  string
	}{
		{out.OriginalFile, []byte("queue test"), "text/plain"},
		{out.Preview, png, "image/png"},
	} {
		if part.link.SizeBytes != int64(len(part.bytes)) || !strings.HasPrefix(part.link.MimeType, part.mime) {
			t.Fatalf("metadata: %#v", part.link)
		}
		expiry, err := time.Parse(time.RFC3339, part.link.ExpiresAt)
		if err != nil || time.Until(expiry) > mcpFileTTL || time.Until(expiry) < mcpFileTTL-time.Second*5 {
			t.Fatalf("expiry: %s, %v", part.link.ExpiresAt, err)
		}
		r, err := http.Get(part.link.URL)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil || r.StatusCode != http.StatusOK || string(body) != string(part.bytes) || r.Header.Get("Cache-Control") != "private, no-store" {
			t.Fatalf("download: status=%d bytes=%d err=%v", r.StatusCode, len(body), err)
		}
	}
	got, err := docs.get(context.Background(), mcpGetArgs{ID: doc.Id})
	if err != nil || got.Text != "OCR text" {
		t.Fatalf("default text: %#v %v", got, err)
	}
	plain, err := newMCPDocs(app, owner).get(context.Background(), mcpGetArgs{ID: doc.Id})
	if err != nil || plain.OriginalFile != nil || plain.Preview != nil {
		t.Fatalf("disabled links: %#v %v", plain, err)
	}
	missingPreview := makeQueueDocument(t, app, owner, "completed", "")
	got, err = docs.get(context.Background(), mcpGetArgs{ID: missingPreview.Id})
	if err != nil || got.OriginalFile == nil || got.Preview != nil {
		t.Fatalf("no preview: %#v %v", got, err)
	}
}

func TestMCPFileLinksRefuseOtherFilesTamperingAndExpiredTokens(t *testing.T) {
	app := bootQueueApp(t)
	owner := makeQueueUser(t, app, "owner-files@example.com")
	other := makeQueueUser(t, app, "other-files@example.com")
	doc := makeQueueDocument(t, app, owner, "completed", "")
	second := makeQueueDocument(t, app, owner, "completed", "")
	foreign := makeQueueDocument(t, app, other, "completed", "")
	srv := mcpFilesServer(t, app)
	app.Settings().Meta.AppURL = srv.URL
	docs := withMCPFileLinks(app, owner, newMCPDocs(app, owner))
	out, err := docs.get(context.Background(), mcpGetArgs{ID: doc.Id})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(out.OriginalFile.URL)
	if err != nil {
		t.Fatal(err)
	}
	token := u.Query().Get("token")
	claims, err := security.ParseUnverifiedJWT(token)
	if err != nil {
		t.Fatal(err)
	}
	user, err := app.FindRecordById("users", owner)
	if err != nil {
		t.Fatal(err)
	}
	claims["exp"] = time.Now().Add(-time.Minute).Unix()
	expired, err := security.NewJWT(claims, mcpFileSigningKey(user), mcpFileTTL)
	if err != nil {
		t.Fatal(err)
	}
	claims["exp"] = time.Now().Add(mcpFileTTL).Unix()
	claims["purpose"] = "file"
	wrongPurpose, err := security.NewJWT(claims, mcpFileSigningKey(user), mcpFileTTL)
	if err != nil {
		t.Fatal(err)
	}
	for label, args := range map[string]struct{ id, field, token string }{
		"same-owner different document": {second.Id, "file", token},
		"foreign document":              {foreign.Id, "file", token},
		"different field":               {doc.Id, "preview", token},
		"unknown field":                 {doc.Id, "../file", token},
		"expired":                       {doc.Id, "file", expired},
		"wrong purpose":                 {doc.Id, "file", wrongPurpose},
		"modified signature":            {doc.Id, "file", token[:len(token)-5] + "xxxxx"},
		"no token":                      {doc.Id, "file", ""},
	} {
		if _, _, err := resolveMCPFile(app, args.id, args.field, args.token); err == nil {
			t.Fatalf("accepted %s", label)
		}
	}
	if _, err := docs.get(context.Background(), mcpGetArgs{ID: foreign.Id}); err == nil {
		t.Fatal("foreign document link issued")
	}
	r, err := http.Get(srv.URL + u.Path)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("bare URL status=%d", r.StatusCode)
	}
	if _, err := app.FindAuthRecordByToken(token, core.TokenTypeAuth); err == nil {
		t.Fatal("file link authenticates as an account")
	}
	if _, err := app.FindAuthRecordByToken(token, core.TokenTypeFile); err == nil {
		t.Fatal("file link works as an account-wide file token")
	}
	user.SetTokenKey("rotated-file-key-at-least-thirty-characters")
	if err := app.Save(user); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveMCPFile(app, doc.Id, "file", token); err == nil {
		t.Fatal("rotated account key accepted")
	}
}

func TestMCPFileLinksRecheckSharingAndCurrentFilename(t *testing.T) {
	app := bootQueueApp(t)
	owner := makeQueueUser(t, app, "owner-share@example.com")
	reader := makeQueueUser(t, app, "reader-share@example.com")
	doc := makeQueueDocument(t, app, owner, "completed", "")
	collection, err := app.FindCollectionByNameOrId(CollectionShares)
	if err != nil {
		t.Fatal(err)
	}
	share := core.NewRecord(collection)
	share.Set("document", doc.Id)
	share.Set("user", reader)
	if err := app.Save(share); err != nil {
		t.Fatal(err)
	}
	app.Settings().Meta.AppURL = "https://archive.example"
	docs := withMCPFileLinks(app, reader, newMCPDocs(app, reader))
	out, err := docs.get(context.Background(), mcpGetArgs{ID: doc.Id})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(out.OriginalFile.URL)
	token := u.Query().Get("token")
	if _, _, err := resolveMCPFile(app, doc.Id, "file", token); err != nil {
		t.Fatal(err)
	}
	if err := app.Delete(share); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveMCPFile(app, doc.Id, "file", token); err == nil {
		t.Fatal("revoked sharing accepted")
	}
	share = core.NewRecord(collection)
	share.Set("document", doc.Id)
	share.Set("user", reader)
	if err := app.Save(share); err != nil {
		t.Fatal(err)
	}
	file, err := filesystem.NewFileFromBytes([]byte("replacement"), "new.txt")
	if err != nil {
		t.Fatal(err)
	}
	doc.Set("file", file)
	if err := app.Save(doc); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveMCPFile(app, doc.Id, "file", token); err == nil {
		t.Fatal("replacement served under old link")
	}
	out, err = docs.get(context.Background(), mcpGetArgs{ID: doc.Id})
	if err != nil {
		t.Fatal(err)
	}
	u, _ = url.Parse(out.OriginalFile.URL)
	if _, _, err := resolveMCPFile(app, doc.Id, "file", u.Query().Get("token")); err != nil {
		t.Fatalf("renewed link: %v", err)
	}
}

func TestMCPFileLinksConfigurationAndApplicationURL(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Setenv("MCP_FILE_LINKS_ENABLED", "")
	if mcpFileLinksFromEnv(log).enabled {
		t.Fatal("default enabled")
	}
	t.Setenv("MCP_FILE_LINKS_ENABLED", "true")
	if !mcpFileLinksFromEnv(log).enabled {
		t.Fatal("enabled flag refused")
	}
	t.Setenv("MCP_FILE_LINKS_ENABLED", "typo")
	if mcpFileLinksFromEnv(log).enabled {
		t.Fatal("invalid flag enabled")
	}
	app := bootQueueApp(t)
	for _, value := range []string{"", "https://user:password@example.com", "ftp://example.com", "https://example.com?token=secret", "https://example.com#fragment", "https://example.com?", "https://example.com#", "/relative"} {
		app.Settings().Meta.AppURL = value
		if _, err := mcpFileBaseURL(app); err == nil {
			t.Fatalf("accepted invalid application URL %s", value)
		}
	}
	app.Settings().Meta.AppURL = " https://archive.example/prefix/ "
	if got, err := mcpFileBaseURL(app); err != nil || got != "https://archive.example/prefix" {
		t.Fatalf("application URL=%s %v", got, err)
	}
	owner := makeQueueUser(t, app, "application-url@example.com")
	doc := makeQueueDocument(t, app, owner, "completed", "")
	docs := withMCPFileLinks(app, owner, newMCPDocs(app, owner))
	out, err := docs.get(context.Background(), mcpGetArgs{ID: doc.Id})
	if err != nil || !strings.HasPrefix(out.OriginalFile.URL, "https://archive.example/prefix/api/") {
		t.Fatalf("initial application URL: %#v %v", out, err)
	}
	app.Settings().Meta.AppURL = "https://updated.example"
	out, err = docs.get(context.Background(), mcpGetArgs{ID: doc.Id})
	if err != nil || !strings.HasPrefix(out.OriginalFile.URL, "https://updated.example/api/") {
		t.Fatalf("updated application URL: %#v %v", out, err)
	}
}

func TestMCPMetadataOnlyPreservesUnicodeLength(t *testing.T) {
	app := bootQueueApp(t)
	owner := makeQueueUser(t, app, "unicode-files@example.com")
	doc := makeQueueDocument(t, app, owner, "completed", "Привет")
	include := false
	out, err := getMCPDocument(app, owner, mcpGetArgs{ID: doc.Id, IncludeText: &include})
	if err != nil || out.Text != "" || out.TextChars != 6 || out.Truncated {
		t.Fatalf("metadata=%#v %v", out, err)
	}
	data, err := json.Marshal(out)
	if err != nil || strings.Contains(string(data), "Привет") {
		t.Fatalf("OCR leaked: %s %v", data, err)
	}
}
