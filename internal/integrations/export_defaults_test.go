package integrations

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/metavert-io/flipbook/internal/models"
)

// canvaExportMock serves a Canva export that completes on the first status
// poll and downloads testPDF.
func canvaExportMock(t *testing.T) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/exports":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"job":{"id":"job-1","status":"in_progress"}}`))
		case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/exports/"):
			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(writer, `{"job":{"id":"job-1","status":"success","urls":[%q]}}`, server.URL+"/files/design.pdf")
		case request.Method == http.MethodGet && request.URL.Path == "/files/design.pdf":
			writer.Header().Set("Content-Type", "application/pdf")
			_, _ = writer.Write(testPDF)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestExportInheritsWorkspaceFlipbookDefaults(t *testing.T) {
	store := newFakeStore()
	settings := models.WorkspaceSettings{
		DefaultVisibility: models.VisibilityUnlisted,
		DefaultDownload:   models.DownloadSettings{Enabled: true, AllowOriginal: true, AllowPDF: true},
		DefaultEmbed:      models.EmbedSettings{Enabled: true, AllowedDomains: []string{"penninn.is"}, StartPage: 2, ShowToolbar: true, ReadingMode: "single"},
		DefaultShare:      models.ShareSettings{Enabled: true, Native: true, LinkedIn: true},
	}
	store.workspaces["workspace"] = &models.Workspace{ID: "workspace", Settings: settings}
	queue := &fakeQueue{}
	service := testService(t, store, queue, ProviderCanva, canvaExportMock(t).URL)
	seedConnection(t, service, store, ProviderCanva, false)

	result, err := service.ExportAndReupload(context.Background(), ExportRequest{
		Provider: ProviderCanva, WorkspaceID: "workspace", UserID: "user", ExternalID: "design-1", Title: "Board report",
	})
	if err != nil {
		t.Fatal(err)
	}
	flipbook := store.flipbooks[result.FlipbookID]
	if flipbook == nil {
		t.Fatalf("export did not create a flipbook: %+v", result)
	}
	if flipbook.Visibility != models.VisibilityUnlisted {
		t.Errorf("Visibility = %q, want the workspace default %q", flipbook.Visibility, models.VisibilityUnlisted)
	}
	if !reflect.DeepEqual(flipbook.Download, settings.DefaultDownload) {
		t.Errorf("Download = %+v, want %+v", flipbook.Download, settings.DefaultDownload)
	}
	if !reflect.DeepEqual(flipbook.Embed, settings.DefaultEmbed) {
		t.Errorf("Embed = %+v, want %+v", flipbook.Embed, settings.DefaultEmbed)
	}
	if !reflect.DeepEqual(flipbook.Share, settings.DefaultShare) {
		t.Errorf("Share = %+v, want %+v", flipbook.Share, settings.DefaultShare)
	}

	// A re-export adds a revision; it must not reset settings an editor has
	// changed since, even if the workspace defaults moved too.
	store.flipbooks[result.FlipbookID].Download = models.DownloadSettings{}
	store.workspaces["workspace"].Settings.DefaultShare = models.ShareSettings{}
	if _, err := service.ExportAndReupload(context.Background(), ExportRequest{
		Provider: ProviderCanva, WorkspaceID: "workspace", UserID: "user", ExternalID: "design-1",
	}); err != nil {
		t.Fatal(err)
	}
	reexported := store.flipbooks[result.FlipbookID]
	if reexported.Download.Enabled || !reexported.Share.Enabled {
		t.Fatalf("re-export overwrote the publication's settings: download=%+v share=%+v", reexported.Download, reexported.Share)
	}
}

func TestExportFailsWithoutWorkspaceBeforeCreatingAFlipbook(t *testing.T) {
	store := newFakeStore()
	delete(store.workspaces, "workspace")
	queue := &fakeQueue{}
	service := testService(t, store, queue, ProviderCanva, canvaExportMock(t).URL)
	seedConnection(t, service, store, ProviderCanva, false)

	_, err := service.ExportAndReupload(context.Background(), ExportRequest{
		Provider: ProviderCanva, WorkspaceID: "workspace", UserID: "user", ExternalID: "design-1",
	})
	if err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("missing workspace did not fail the export: %v", err)
	}
	if len(store.flipbooks) != 0 || len(queue.jobs) != 0 {
		t.Fatalf("a flipbook or job was created without workspace defaults: flipbooks=%d jobs=%d", len(store.flipbooks), len(queue.jobs))
	}
}
