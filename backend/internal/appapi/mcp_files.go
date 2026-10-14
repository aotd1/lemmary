package appapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
	"github.com/pocketbase/pocketbase/tools/security"
)

const (
	mcpFileRoute   = "/api/app/mcp/files/{documentId}/{field}"
	mcpFilePurpose = "mcp-document-file"
	mcpFileTTL     = 3 * time.Minute
)

type mcpFileLink struct {
	URL       string `json:"url"`
	Filename  string `json:"filename"`
	MimeType  string `json:"mime_type"`
	SizeBytes int64  `json:"size_bytes"`
	ExpiresAt string `json:"expires_at"`
}

type mcpFileLinksConfig struct {
	enabled bool
	baseURL string
}

func mcpFileLinksFromEnv(log *slog.Logger) mcpFileLinksConfig {
	cfg := mcpFileLinksConfig{}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MCP_FILE_LINKS_ENABLED"))) {
	case "", "0", "false", "no", "off":
		return cfg
	case "1", "true", "yes", "on":
		cfg.enabled = true
	default:
		log.Error("MCP file links disabled: MCP_FILE_LINKS_ENABLED must be a boolean")
		return cfg
	}
	cfg.baseURL = strings.TrimRight(strings.TrimSpace(os.Getenv("MCP_PUBLIC_URL")), "/")
	if cfg.baseURL != "" {
		u, err := url.Parse(cfg.baseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(cfg.baseURL, "#") {
			log.Error("MCP file links disabled: MCP_PUBLIC_URL must be an HTTP(S) base URL without credentials, query or fragment")
			return mcpFileLinksConfig{}
		}
	}
	return cfg
}

func mcpFileBaseURL(e *core.RequestEvent, configured string) string {
	if configured != "" {
		return configured
	}
	scheme := "http"
	if e.IsTLS() {
		scheme = "https"
	}
	return scheme + "://" + e.Request.Host
}

func withMCPFileLinks(app core.App, userID string, docs mcpDocs, baseURL string) mcpDocs {
	get := docs.get
	docs.get = func(ctx context.Context, args mcpGetArgs) (mcpGetResult, error) {
		out, err := get(ctx, args)
		if err != nil {
			return out, err
		}
		record, err := app.FindRecordById("documents", out.ID)
		if err != nil || !CanReadDocument(app, record, userID) {
			return mcpGetResult{}, fmt.Errorf("no document %s", out.ID)
		}
		user, err := app.FindRecordById("users", userID)
		if err != nil {
			return mcpGetResult{}, err
		}
		fsys, err := app.NewFilesystem()
		if err != nil {
			return mcpGetResult{}, err
		}
		defer fsys.Close()
		out.OriginalFile, err = newMCPFileLink(fsys, user, record, "file", baseURL)
		if err == nil {
			out.Preview, err = newMCPFileLink(fsys, user, record, "preview", baseURL)
		}
		return out, err
	}
	return docs
}

func mcpFileSigningKey(user *core.Record) string {
	return user.TokenKey() + user.Collection().FileToken.Secret + ":" + mcpFilePurpose
}

func newMCPFileLink(fsys *filesystem.System, user, record *core.Record, field, baseURL string) (*mcpFileLink, error) {
	name := record.GetString(field)
	if name == "" {
		return nil, nil
	}
	attrs, err := fsys.Attributes(record.BaseFilesPath() + "/" + name)
	if err != nil {
		return nil, err
	}
	expires := time.Now().Add(mcpFileTTL).Unix()
	token, err := security.NewJWT(jwt.MapClaims{
		"purpose": mcpFilePurpose, "uid": user.Id, "doc": record.Id,
		"field": field, "filename": name, "exp": expires,
	}, mcpFileSigningKey(user), mcpFileTTL)
	if err != nil {
		return nil, err
	}
	path := "/api/app/mcp/files/" + url.PathEscape(record.Id) + "/" + field
	return &mcpFileLink{
		URL: baseURL + path + "?token=" + url.QueryEscape(token), Filename: name,
		MimeType: attrs.ContentType, SizeBytes: attrs.Size,
		ExpiresAt: time.Unix(expires, 0).UTC().Format(time.RFC3339),
	}, nil
}

func resolveMCPFile(app core.App, documentID, field, token string) (*core.Record, string, error) {
	if field != "file" && field != "preview" {
		return nil, "", fmt.Errorf("invalid file field")
	}
	claims, err := security.ParseUnverifiedJWT(token)
	if err != nil {
		return nil, "", err
	}
	uid, _ := claims["uid"].(string)
	user, err := app.FindRecordById("users", uid)
	if err != nil {
		return nil, "", err
	}
	claims, err = security.ParseJWT(token, mcpFileSigningKey(user))
	if err != nil || claims["purpose"] != mcpFilePurpose || claims["doc"] != documentID || claims["field"] != field {
		return nil, "", fmt.Errorf("invalid file token")
	}
	record, err := app.FindRecordById("documents", documentID)
	if err != nil || !CanReadDocument(app, record, uid) {
		return nil, "", fmt.Errorf("document is not readable")
	}
	name := record.GetString(field)
	if name == "" || claims["filename"] != name {
		return nil, "", fmt.Errorf("file is no longer available")
	}
	return record, name, nil
}

func handleMCPFile(app core.App) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		e.Response.Header().Set("Cache-Control", "private, no-store")
		e.Response.Header().Set("Referrer-Policy", "no-referrer")
		record, name, err := resolveMCPFile(app, e.Request.PathValue("documentId"), e.Request.PathValue("field"), e.Request.URL.Query().Get("token"))
		if err != nil {
			return writeError(e, http.StatusNotFound, "Not found.")
		}
		fsys, err := app.NewFilesystem()
		if err != nil {
			return err
		}
		defer fsys.Close()
		return fsys.Serve(e.Response, e.Request, record.BaseFilesPath()+"/"+name, name)
	}
}
