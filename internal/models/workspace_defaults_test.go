package models

import (
	"reflect"
	"testing"
)

// fullyPopulatedSettings sets every field of the inherited settings blocks to
// a non-zero value. requireNonZeroFields fails if a field is added to one of
// these structs without being filled in here, so the DeepEqual checks below
// keep covering every field.
func fullyPopulatedSettings() WorkspaceSettings {
	return WorkspaceSettings{
		DefaultVisibility: VisibilityUnlisted,
		DefaultDownload:   DownloadSettings{Enabled: true, AllowOriginal: true, AllowPDF: true, AllowPrint: true},
		DefaultEmbed: EmbedSettings{
			Enabled: true, AllowedDomains: []string{"penninn.is", "example.com"}, StartPage: 3,
			ShowToolbar: true, ShowSearch: true, ShowShare: true, ShowDownload: true,
			ReadingMode: "single", Background: "#112233",
		},
		DefaultShare: ShareSettings{Enabled: true, Native: true, Email: true, Facebook: true, LinkedIn: true, X: true},
	}
}

func requireNonZeroFields(t *testing.T, name string, value any) {
	t.Helper()
	v := reflect.ValueOf(value)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Fatalf("test fixture %s.%s is zero; set it so the copy is actually checked", name, v.Type().Field(i).Name)
		}
	}
}

func TestNewFlipbookDefaultsCopiesEveryField(t *testing.T) {
	settings := fullyPopulatedSettings()
	requireNonZeroFields(t, "DownloadSettings", settings.DefaultDownload)
	requireNonZeroFields(t, "EmbedSettings", settings.DefaultEmbed)
	requireNonZeroFields(t, "ShareSettings", settings.DefaultShare)
	workspace := &Workspace{ID: "ws", Settings: settings}

	defaults := workspace.NewFlipbookDefaults()
	requireNonZeroFields(t, "FlipbookDefaults", defaults)
	if defaults.Visibility != settings.DefaultVisibility {
		t.Errorf("Visibility = %q, want %q", defaults.Visibility, settings.DefaultVisibility)
	}
	if !reflect.DeepEqual(defaults.Download, settings.DefaultDownload) {
		t.Errorf("Download = %+v, want %+v", defaults.Download, settings.DefaultDownload)
	}
	if !reflect.DeepEqual(defaults.Embed, settings.DefaultEmbed) {
		t.Errorf("Embed = %+v, want %+v", defaults.Embed, settings.DefaultEmbed)
	}
	if !reflect.DeepEqual(defaults.Share, settings.DefaultShare) {
		t.Errorf("Share = %+v, want %+v", defaults.Share, settings.DefaultShare)
	}

	fb := &Flipbook{ID: "fb"}
	defaults.Apply(fb)
	if fb.Visibility != settings.DefaultVisibility {
		t.Errorf("applied Visibility = %q, want %q", fb.Visibility, settings.DefaultVisibility)
	}
	if !reflect.DeepEqual(fb.Download, settings.DefaultDownload) ||
		!reflect.DeepEqual(fb.Embed, settings.DefaultEmbed) ||
		!reflect.DeepEqual(fb.Share, settings.DefaultShare) {
		t.Errorf("applied settings differ from workspace defaults: download=%+v embed=%+v share=%+v", fb.Download, fb.Embed, fb.Share)
	}

	fb.Embed.AllowedDomains[0] = "changed.example"
	if workspace.Settings.DefaultEmbed.AllowedDomains[0] != "penninn.is" {
		t.Fatal("editing the flipbook's embed allowlist changed the workspace default")
	}
}

func TestFlipbookDefaultsApplyKeepsCallerVisibility(t *testing.T) {
	workspace := &Workspace{Settings: fullyPopulatedSettings()}
	fb := &Flipbook{Visibility: VisibilityPrivate}
	workspace.NewFlipbookDefaults().Apply(fb)
	if fb.Visibility != VisibilityPrivate {
		t.Fatalf("caller-chosen visibility was overwritten: %q", fb.Visibility)
	}
	if !fb.Download.Enabled || !fb.Share.Enabled || !fb.Embed.Enabled {
		t.Fatalf("download/share/embed were not applied alongside a caller visibility: %+v", fb)
	}
}

func TestNewFlipbookDefaultsFallbacks(t *testing.T) {
	unset := (&Workspace{ID: "legacy"}).NewFlipbookDefaults()
	if unset.Visibility != VisibilityPublic {
		t.Fatalf("empty workspace visibility should fall back to public, got %q", unset.Visibility)
	}

	var missing *Workspace
	got := missing.NewFlipbookDefaults()
	want := FlipbookDefaults{Visibility: VisibilityPublic, Embed: DefaultEmbedSettings()}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(FallbackFlipbookDefaults(), want) {
		t.Fatalf("fallback defaults = %+v, want %+v", got, want)
	}
	if got.Download.Enabled || got.Share.Enabled {
		t.Fatal("fallback defaults must not turn on downloads or sharing")
	}
}
