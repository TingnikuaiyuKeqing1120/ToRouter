package console

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"testing"

	"torouter/internal/management"
	"torouter/internal/store"
)

func TestMCPNativeCredentialZIPRoundTrip(t *testing.T) {
	f := newDiscoveryConsoleFixture(t)
	c := transferTestChannel(t, f.store, "zip-one", "zip-two")
	owner, _ := f.client(t, management.Read, management.Manage, management.Sensitive)
	refs := []store.OAuthTransferRef{}
	for _, item := range c.Credentials {
		refs = append(refs, store.OAuthTransferRef{ChannelID: c.ID, CredentialID: item.ID})
	}
	exported := dataCall(t, owner, "export_oauth_credentials", map[string]any{"refs": refs})
	var artifact management.Artifact
	discoveryDecode(t, exported["artifact"], &artifact)
	if artifact.MIMEType != "application/zip" || exported["count"] != float64(2) {
		t.Fatal("missing native ZIP artifact")
	}
	var content bytes.Buffer
	for offset := 0; offset < artifact.Size; {
		chunk := dataCall(t, owner, "read_export", map[string]any{"id": artifact.ID, "offset": offset, "limit": 19})
		if chunk["encoding"] != "base64" {
			t.Fatal("ZIP must be byte-safe")
		}
		part, err := base64.StdEncoding.DecodeString(chunk["content"].(string))
		if err != nil {
			t.Fatal(err)
		}
		next := int(chunk["next_offset"].(float64))
		if next-offset != len(part) || len(part) > 19 {
			t.Fatal("ZIP offset is not original bytes")
		}
		content.Write(part)
		offset = next
	}
	reader, err := zip.NewReader(bytes.NewReader(content.Bytes()), int64(content.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(reader.File) != 2 {
		t.Fatal("ZIP did not contain exactly two native files")
	}
	for i, file := range reader.File {
		r, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		var native struct {
			Tokens struct {
				Access  string `json:"access_token"`
				Refresh string `json:"refresh_token"`
			} `json:"tokens"`
		}
		if json.Unmarshal(raw, &native) != nil || native.Tokens.Access != c.Credentials[i].OAuth.AccessToken || native.Tokens.Refresh != c.Credentials[i].OAuth.RefreshToken {
			t.Fatal("ZIP native credentials changed")
		}
	}
}
