package handlers

import (
	"reflect"
	"testing"

	"github.com/metavert-io/flipbook/internal/models"
	"github.com/metavert-io/flipbook/internal/storage"
)

func apiDefaultsWorkspace() *models.Workspace {
	return &models.Workspace{ID: "ws", Settings: models.WorkspaceSettings{
		DefaultVisibility: models.VisibilityUnlisted,
		DefaultDownload:   models.DownloadSettings{Enabled: true, AllowPDF: true},
		DefaultEmbed:      models.EmbedSettings{Enabled: true, AllowedDomains: []string{"penninn.is"}, StartPage: 2, ShowToolbar: true},
		DefaultShare:      models.ShareSettings{Enabled: true, Email: true, LinkedIn: true},
	}}
}

func TestNewAPIFlipbookInheritsWorkspaceDefaults(t *testing.T) {
	workspace := apiDefaultsWorkspace()
	identity := &apiIdentity{UserID: "user", WorkspaceID: workspace.ID}
	upload := storage.UploadInfo{Size: 42, SHA256: "abc"}
	metadata := uploadMetadata{Title: " Annual report ", Language: "is", ExternalSource: "cms", ExternalID: "42"}

	fb := newAPIFlipbook(workspace, identity, "fb-1", "report.pdf", "annual-report", upload, []string{"finance"}, metadata)

	if fb.Visibility != models.VisibilityUnlisted {
		t.Errorf("Visibility = %q, want the workspace default %q", fb.Visibility, models.VisibilityUnlisted)
	}
	if !reflect.DeepEqual(fb.Download, workspace.Settings.DefaultDownload) {
		t.Errorf("Download = %+v, want %+v", fb.Download, workspace.Settings.DefaultDownload)
	}
	if !reflect.DeepEqual(fb.Embed, workspace.Settings.DefaultEmbed) {
		t.Errorf("Embed = %+v, want %+v", fb.Embed, workspace.Settings.DefaultEmbed)
	}
	if !reflect.DeepEqual(fb.Share, workspace.Settings.DefaultShare) {
		t.Errorf("Share = %+v, want %+v", fb.Share, workspace.Settings.DefaultShare)
	}
	if fb.ID != "fb-1" || fb.WorkspaceID != "ws" || fb.OwnerID != "user" || fb.Title != "Annual report" ||
		fb.Slug != "annual-report" || fb.Filename != "report.pdf" || fb.FileSize != 42 || fb.SourceChecksum != "abc" ||
		fb.Status != models.StatusPending || fb.PublicationState != models.PublicationDraft ||
		fb.Language != "is" || fb.ExternalSource != "cms" || fb.ExternalID != "42" || len(fb.Tags) != 1 {
		t.Errorf("request metadata was not carried onto the flipbook: %+v", fb)
	}
}

func TestNewAPIFlipbookKeepsCallerVisibility(t *testing.T) {
	workspace := apiDefaultsWorkspace()
	identity := &apiIdentity{UserID: "user", WorkspaceID: workspace.ID}
	metadata := uploadMetadata{Title: "Draft", Visibility: models.VisibilityPrivate}

	fb := newAPIFlipbook(workspace, identity, "fb-2", "draft.pdf", "draft", storage.UploadInfo{}, nil, metadata)

	if fb.Visibility != models.VisibilityPrivate {
		t.Fatalf("caller visibility was replaced by the workspace default: %q", fb.Visibility)
	}
	if !fb.Download.Enabled || !fb.Share.Enabled || !fb.Embed.Enabled {
		t.Fatalf("workspace download/share/embed defaults were not applied: %+v", fb)
	}
}
