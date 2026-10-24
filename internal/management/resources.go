package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"torouter/internal/store"
)

type Artifact struct {
	ID        string    `json:"id"`
	URI       string    `json:"uri"`
	Name      string    `json:"name"`
	MIMEType  string    `json:"mime_type"`
	ExpiresAt time.Time `json:"expires_at"`
	Size      int       `json:"size"`
}
type ArtifactContent struct {
	Artifact
	Content []byte `json:"-"`
}

func (s *Server) PutArtifact(ctx context.Context, p Principal, name, mimeType string, content []byte) (Artifact, error) {
	if err := p.Require(ctx, Read); err != nil {
		return Artifact{}, err
	}
	if name == "" || len(name) > 100 || strings.ContainsAny(name, "/\\\r\n") {
		return Artifact{}, NewError("invalid_arguments", "Export name must be a short display filename without a path.")
	}
	mediaType, _, err := mime.ParseMediaType(mimeType)
	if err != nil || (mediaType != "text/csv" && mediaType != "text/plain" && mediaType != "application/json" && mediaType != "application/zip") {
		return Artifact{}, NewError("invalid_arguments", "Supported export types: text/csv, text/plain, application/json, application/zip.")
	}
	permissions := []string{Read}
	if p.Has(Sensitive) {
		if err = p.Require(ctx, Sensitive); err != nil {
			return Artifact{}, err
		}
		permissions = append(permissions, Sensitive)
	}
	a, err := s.store.PutManagementArtifact(ctx, p.ID, name, mimeType, content, permissions)
	if errors.Is(err, store.ErrManagementArtifactLimit) {
		return Artifact{}, NewError("limit_exceeded", "Exports are limited to 16 MiB each and 20 active exports per authorization. Narrow the range or wait for expiry.")
	}
	if err != nil {
		return Artifact{}, err
	}
	return artifactMetadata(a), nil
}

func artifactMetadata(a store.ManagementArtifact) Artifact {
	return Artifact{ID: a.ID, URI: "torouter://exports/" + a.ID, Name: a.Name, MIMEType: a.MIMEType, ExpiresAt: a.ExpiresAt, Size: a.Size}
}

func (s *Server) ReadArtifact(ctx context.Context, p Principal, id string) (ArtifactContent, error) {
	if p.audit == nil {
		raw, _ := json.Marshal(map[string]string{"id": id})
		p.audit = safeAuditTarget("read_export", raw)
	}
	if err := p.Require(ctx, Read); err != nil {
		return ArtifactContent{}, err
	}
	a, err := s.store.ReadManagementArtifact(ctx, p.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		return ArtifactContent{}, NewError("not_found", "Export is expired or unavailable to this authorization.")
	}
	if err != nil {
		return ArtifactContent{}, err
	}
	for _, permission := range a.Permissions {
		if err = p.Require(ctx, permission); err != nil {
			return ArtifactContent{}, err
		}
	}
	return ArtifactContent{Artifact: artifactMetadata(a), Content: a.Content}, nil
}

func (s *Server) addResources(server *mcp.Server, p Principal) {
	if !p.Has(Read) {
		return
	}
	server.AddResource(&mcp.Resource{URI: "torouter://help", Name: "to-router management help", MIMEType: "text/markdown", Description: "Quick start and current authorization's registered tool reference."}, func(ctx context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		if err := p.Require(ctx, Read); err != nil {
			return nil, publicError(err)
		}
		var text strings.Builder
		text.WriteString("# to-router 管理接入\n\n先检索对象，再使用精确 ID；未知值不能当成 0。修改前读取当前版本。普通写操作使用唯一 request_id；相同 ID 必须保持参数相同。断线或 unknown 后先读取回执和当前状态，不能新建 ID 盲重试。删除、覆盖、清理和历史更正必须使用绑定目标快照的维护预览。维护与敏感读取分别授权；授权根只能由用户在控制台管理。\n\nHTTP 地址或端口改变后，请从独立的MCP控制台获取实际地址并重新连接。stdio 只连接已运行网关，使用 TOROUTER_MCP_URL 和 TOROUTER_MCP_TOKEN。导出资源一小时失效，绑定当前授权与所需权限。\n\n## 当前可用工具\n\n")
		for _, op := range s.operations {
			if err := p.Require(ctx, op.Permission); err == nil {
				fmt.Fprintf(&text, "- `%s` [%s] — %s\n", op.Name, op.Permission, op.Description)
			}
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "torouter://help", MIMEType: "text/markdown", Text: text.String()}}}, nil
	})
	server.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "torouter://exports/{id}", Name: "Management export", Description: "Read a bounded export created by this authorization; expires in one hour and rechecks required permissions."}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		id, ok := strings.CutPrefix(req.Params.URI, "torouter://exports/")
		if !ok || strings.ContainsAny(id, "/?#") {
			return nil, NewError("not_found", "Unknown export resource.")
		}
		a, err := s.ReadArtifact(ctx, p, id)
		if err != nil {
			return nil, publicError(err)
		}
		contents := &mcp.ResourceContents{URI: a.URI, MIMEType: a.MIMEType}
		mediaType, _, _ := mime.ParseMediaType(a.MIMEType)
		if mediaType != "application/zip" && utf8.Valid(a.Content) {
			contents.Text = string(a.Content)
		} else {
			contents.Blob = a.Content
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{contents}}, nil
	})
}
