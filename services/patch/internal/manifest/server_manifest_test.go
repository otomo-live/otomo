package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

const clientDoc = `{"format":1,"channel":"live","release_id":9007199254740993,"created_at":"2026-09-29T00:00:00Z","min_client_version":"1.0.0","config":{"ui.motd":{"version":1,"sha256":"ab","size":1}},"packs":[{"name":"p","sha256":"cd","size":2}]}`

func hashOf(t *testing.T, raw string) string {
	t.Helper()
	body, err := Canonical([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// TestEmptyServerManifestKeepsTheReleaseFields: a NULL server manifest becomes the
// client manifest's own fields with an empty config and no packs, canonical, with an
// ETag of its own. The release id above 2^53 survives, since values are never parsed
// as floats.
func TestEmptyServerManifestKeepsTheReleaseFields(t *testing.T) {
	doc, err := EmptyServerManifest([]byte(clientDoc))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"channel":"live","config":{},"created_at":"2026-09-29T00:00:00Z","format":1,"min_client_version":"1.0.0","packs":[],"release_id":9007199254740993}`
	if string(doc.Body) != want {
		t.Errorf("body = %s\nwant   %s", doc.Body, want)
	}
	sum := sha256.Sum256(doc.Body)
	if doc.ETag != `"`+hex.EncodeToString(sum[:])+`"` {
		t.Errorf("ETag %s is not the body's SHA-256", doc.ETag)
	}
}

func TestBuildEntryWithAStoredServerManifest(t *testing.T) {
	server := `{"format":1,"channel":"live","release_id":7,"created_at":"x","min_client_version":"1.0.0","config":{"session.rules":{"version":2,"sha256":"ef","size":3}},"packs":[]}`
	serverSum := hashOf(t, server)

	e, err := buildEntry("live", 7, clientDoc, hashOf(t, clientDoc), "1.0.0", &server, &serverSum)
	if err != nil {
		t.Fatal(err)
	}
	if e.Server.ETag != `"`+serverSum+`"` || !strings.Contains(string(e.Server.Body), "session.rules") {
		t.Errorf("server = %s %s", e.Server.ETag, e.Server.Body)
	}
	if strings.Contains(string(e.Body), "session.rules") {
		t.Error("the server namespace leaked into the client manifest")
	}
}

// TestBuildEntryRejectsABadServerManifest: a server manifest that does not hash back to
// its stored sum rejects the whole row, so the channel keeps its last good release.
func TestBuildEntryRejectsABadServerManifest(t *testing.T) {
	server := `{"config":{}}`
	wrong := strings.Repeat("0", 64)
	if _, err := buildEntry("live", 7, clientDoc, hashOf(t, clientDoc), "1.0.0", &server, &wrong); err == nil {
		t.Fatal("a server manifest with the wrong hash was accepted")
	}
}

func TestBuildEntryWithANullServerManifest(t *testing.T) {
	e, err := buildEntry("live", 7, clientDoc, hashOf(t, clientDoc), "1.0.0", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(e.Server.Body), `"config":{}`) || e.Server.ETag == e.ETag {
		t.Errorf("server = %s %s", e.Server.ETag, e.Server.Body)
	}
}
