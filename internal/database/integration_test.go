package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/metavert-io/flipbook/internal/models"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func integrationURI(t *testing.T) string {
	t.Helper()
	uri := os.Getenv("FLIPBOOK_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("FLIPBOOK_TEST_MONGO_URI is not set; skipping MongoDB integration test")
	}
	return uri
}

func integrationDatabaseName() string {
	return "flipbook_test_" + strings.ReplaceAll(newID(), "-", "")
}

func openIntegrationDB(t *testing.T, uri, name string) *DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := Open(ctx, uri, name)
	if err != nil {
		t.Fatalf("open integration database: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		_ = db.database.Drop(cleanupCtx)
		_ = db.Close(cleanupCtx)
	})
	return db
}

func TestIntegrationMigrationPreservesLegacyData(t *testing.T) {
	uri := integrationURI(t)
	name := integrationDatabaseName()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	rawDB := client.Database(name)
	now := time.Now().UTC()
	converted := now.Add(-time.Hour)
	_, err = rawDB.Collection("flipbooks").InsertMany(ctx, []any{
		bson.M{
			"_id": "legacy-public", "title": "Legacy public", "slug": "legacy-public",
			"filename": "public.pdf", "status": models.StatusReady, "is_public": true,
			"created_at": now.Add(-2 * time.Hour), "updated_at": now, "converted_at": converted,
		},
		bson.M{
			"_id": "legacy-private", "title": "Legacy private", "slug": "legacy-private",
			"filename": "private.pdf", "status": models.StatusReady, "is_public": false,
			"created_at": now.Add(-2 * time.Hour), "updated_at": now, "converted_at": converted,
		},
	})
	if err != nil {
		_ = client.Disconnect(ctx)
		t.Fatal(err)
	}
	_, err = rawDB.Collection("flipbooks").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "slug", Value: 1}}, Options: options.Index().SetUnique(true),
	})
	if err != nil {
		_ = client.Disconnect(ctx)
		t.Fatal(err)
	}
	if err := client.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}

	db := openIntegrationDB(t, uri, name)
	public, err := db.GetFlipbook("legacy-public")
	if err != nil {
		t.Fatal(err)
	}
	if public.PublicationState != models.PublicationPublished || public.Visibility != models.VisibilityPublic ||
		!public.IsPublic || public.WorkspaceID != models.DefaultWorkspaceID || public.PublishedAt == nil ||
		public.PublishedAt.Unix() != converted.Unix() {
		t.Fatalf("public legacy document migrated incorrectly: %+v", public)
	}
	private, err := db.GetFlipbook("legacy-private")
	if err != nil {
		t.Fatal(err)
	}
	if private.PublicationState != models.PublicationUnpublished || private.Visibility != models.VisibilityPrivate || private.IsPublic {
		t.Fatalf("private legacy document migrated incorrectly: %+v", private)
	}
	revision, err := db.GetCurrentRevision(ctx, models.DefaultWorkspaceID, public.ID)
	if err != nil || revision.Status != models.RevisionActive {
		t.Fatalf("legacy active revision missing: %+v, %v", revision, err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migration is not idempotent: %v", err)
	}
	if err := db.EnsureIndexes(ctx); err != nil {
		t.Fatalf("index creation is not idempotent: %v", err)
	}
	cursor, err := db.flipbooks.Indexes().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cursor.Close(ctx)
	foundCompoundSlug := false
	for cursor.Next(ctx) {
		var spec struct {
			Name string `bson:"name"`
		}
		if err := cursor.Decode(&spec); err != nil {
			t.Fatal(err)
		}
		if spec.Name == "slug_1" {
			t.Fatal("legacy global slug index was not removed")
		}
		if spec.Name == "workspace_id_1_slug_1" {
			foundCompoundSlug = true
		}
	}
	if !foundCompoundSlug {
		t.Fatal("workspace-scoped slug index missing")
	}
}

func TestIntegrationFoundationWorkflows(t *testing.T) {
	uri := integrationURI(t)
	db := openIntegrationDB(t, uri, integrationDatabaseName())
	ctx := context.Background()

	fb := &models.Flipbook{Title: "Draft publication", Slug: "draft-publication", Status: models.StatusPending}
	if err := db.CreateFlipbookContext(ctx, fb); err != nil {
		t.Fatal(err)
	}
	if fb.PublicationState != models.PublicationDraft || fb.IsPublic || fb.CurrentRevisionID == "" {
		t.Fatalf("new publication defaults incorrect: %+v", fb)
	}
	firstRevisionID := fb.CurrentRevisionID
	if err := db.MarkRevisionReady(ctx, firstRevisionID, "manifest-v1", 2, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.ActivateRevision(ctx, fb.WorkspaceID, fb.ID, firstRevisionID); err != nil {
		t.Fatal(err)
	}
	secondRevision := &models.Revision{WorkspaceID: fb.WorkspaceID, FlipbookID: fb.ID, SourceFilename: "replacement.pdf"}
	if err := db.CreateRevision(ctx, secondRevision); err != nil {
		t.Fatal(err)
	}
	if secondRevision.Number != 2 || secondRevision.AssetVersion != 2 {
		t.Fatalf("revision sequence incorrect: %+v", secondRevision)
	}
	if err := db.MarkRevisionReady(ctx, secondRevision.ID, "manifest-v2", 3, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.ActivateRevision(ctx, fb.WorkspaceID, fb.ID, secondRevision.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.ActivateRevision(ctx, fb.WorkspaceID, fb.ID, firstRevisionID); err != nil {
		t.Fatalf("rollback to superseded revision failed: %v", err)
	}
	current, err := db.GetFlipbook(fb.ID)
	if err != nil || current.CurrentRevisionID != firstRevisionID || current.AssetVersion != 1 {
		t.Fatalf("revision rollback pointer incorrect: %+v, %v", current, err)
	}

	if err := db.ChangeFlipbookSlug(ctx, fb.WorkspaceID, fb.ID, "renamed-publication"); err != nil {
		t.Fatal(err)
	}
	resolved, redirected, err := db.GetFlipbookBySlugOrRedirect(ctx, fb.WorkspaceID, "draft-publication")
	if err != nil || !redirected || resolved.ID != fb.ID {
		t.Fatalf("permanent slug redirect failed: %+v, %v, %v", resolved, redirected, err)
	}
	if err := db.ChangeFlipbookSlug(ctx, fb.WorkspaceID, fb.ID, "draft-publication"); !errors.Is(err, ErrConflict) {
		t.Fatalf("redirect slug was reusable, expected conflict, got %v", err)
	}

	accessToken := &models.AccessToken{Name: "Reader link", TokenHash: models.HashToken("reader-secret")}
	if err := db.CreateFlipbookAccessToken(ctx, fb.WorkspaceID, fb.ID, accessToken); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetFlipbookByAccessToken(ctx, "reader-secret", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeFlipbookAccessToken(ctx, fb.WorkspaceID, fb.ID, accessToken.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetFlipbookByAccessToken(ctx, "reader-secret", time.Now()); !errors.Is(err, mongo.ErrNoDocuments) {
		t.Fatalf("revoked access token still works: %v", err)
	}

	job := &models.Job{WorkspaceID: fb.WorkspaceID, FlipbookID: fb.ID, Type: models.JobConvert, IdempotencyKey: "conversion-1", MaxAttempts: 2}
	if err := db.EnqueueJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	jobID := job.ID
	duplicate := &models.Job{WorkspaceID: fb.WorkspaceID, Type: models.JobConvert, IdempotencyKey: "conversion-1"}
	if err := db.EnqueueJob(ctx, duplicate); err != nil || duplicate.ID != jobID {
		t.Fatalf("idempotent enqueue returned a different job: %+v, %v", duplicate, err)
	}
	leased, err := db.LeaseJobs(ctx, "worker-1", nil, 1, time.Minute)
	if err != nil || len(leased) != 1 || leased[0].Attempts != 1 {
		t.Fatalf("job lease failed: %+v, %v", leased, err)
	}
	if err := db.FailJob(ctx, jobID, "worker-1", "temporary", time.Millisecond, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := db.jobs.UpdateByID(ctx, jobID, bson.M{"$set": bson.M{"run_at": time.Now().Add(-time.Second)}}); err != nil {
		t.Fatal(err)
	}
	leased, err = db.LeaseJobs(ctx, "worker-1", nil, 1, time.Minute)
	if err != nil || len(leased) != 1 || leased[0].Attempts != 2 {
		t.Fatalf("job retry lease failed: %+v, %v", leased, err)
	}
	if err := db.CompleteJob(ctx, jobID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	job, err = db.GetJob(ctx, jobID)
	if err != nil || job.Status != models.JobSucceeded {
		t.Fatalf("job did not complete: %+v, %v", job, err)
	}

	writeOptions := AnalyticsWriteOptions{Retention: time.Hour, ConsentPolicy: "required", Consented: true}
	for _, event := range []*models.AnalyticsEvent{
		{WorkspaceID: fb.WorkspaceID, FlipbookID: fb.ID, Type: models.AnalyticsQualifiedRead, ReaderIDHash: "reader-1"},
		{WorkspaceID: fb.WorkspaceID, FlipbookID: fb.ID, Type: models.AnalyticsCompletion, ReaderIDHash: "reader-1"},
	} {
		if err := db.InsertAnalyticsEvent(ctx, event, writeOptions); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.InsertAnalyticsEvent(ctx, &models.AnalyticsEvent{
		WorkspaceID: fb.WorkspaceID, FlipbookID: fb.ID, Type: models.AnalyticsQualifiedRead, ReaderIDHash: "reader-2",
	}, AnalyticsWriteOptions{Retention: time.Hour, ConsentPolicy: "required", Consented: false}); err != nil {
		t.Fatal(err)
	}
	summary, err := db.AggregateAnalytics(ctx, models.AnalyticsFilter{WorkspaceID: fb.WorkspaceID})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Events != 3 || summary.QualifiedReads != 2 || summary.UniqueReaders != 1 || summary.CompletionRate != 0.5 {
		t.Fatalf("analytics aggregation incorrect: %+v", summary)
	}

	lead := &models.Lead{WorkspaceID: fb.WorkspaceID, FlipbookID: fb.ID, Email: "Reader@Example.com", Name: "First", Consented: true}
	if err := db.UpsertLead(ctx, lead); err != nil {
		t.Fatal(err)
	}
	lead = &models.Lead{WorkspaceID: fb.WorkspaceID, FlipbookID: fb.ID, Email: "reader@example.com", Name: "Updated", Consented: true}
	if err := db.UpsertLead(ctx, lead); err != nil {
		t.Fatalf("lead upsert failed: %v", err)
	}
	leadCount, err := db.CountLeads(ctx, fb.WorkspaceID, fb.ID)
	if err != nil || leadCount != 1 {
		t.Fatalf("lead upsert duplicated records: %d, %v", leadCount, err)
	}

	confirmationExpiry := time.Now().Add(time.Hour)
	subscriber := &models.Subscriber{
		WorkspaceID: fb.WorkspaceID, Email: "reader@example.com", Status: models.SubscriberPending,
		ConfirmationTokenHash: models.HashToken("confirm"), UnsubscribeTokenHash: models.HashToken("unsubscribe"),
		ConfirmationExpiresAt: &confirmationExpiry,
	}
	if err := db.UpsertSubscriber(ctx, subscriber); err != nil {
		t.Fatal(err)
	}
	if err := db.ConfirmSubscriber(ctx, "confirm"); err != nil {
		t.Fatal(err)
	}
	subscriber, err = db.GetSubscriber(ctx, fb.WorkspaceID, "reader@example.com")
	if err != nil || subscriber.Status != models.SubscriberSubscribed {
		t.Fatalf("double opt-in failed: %+v, %v", subscriber, err)
	}
	if err := db.UnsubscribeSubscriber(ctx, "unsubscribe"); err != nil {
		t.Fatal(err)
	}

	purchase := &models.Purchase{
		WorkspaceID: fb.WorkspaceID, FlipbookID: fb.ID, UserID: "reader-user",
		AmountCents: 1000, Currency: "USD", StripeCheckoutSessionID: "checkout-1",
	}
	if err := db.CreatePurchase(ctx, purchase); err != nil {
		t.Fatal(err)
	}
	entitlement := &models.Entitlement{}
	if err := db.MarkPurchasePaid(ctx, purchase.ID, "event-1", entitlement); err != nil {
		t.Fatal(err)
	}
	hasAccess, err := db.HasEntitlement(ctx, fb.WorkspaceID, fb.ID, "reader-user", "", time.Now())
	if err != nil || !hasAccess {
		t.Fatalf("paid entitlement missing: %v, %v", hasAccess, err)
	}
	if err := db.MarkPurchasePaid(ctx, purchase.ID, "event-1", &models.Entitlement{}); err != nil {
		t.Fatalf("payment replay failed: %v", err)
	}
	if err := db.RevokeEntitlement(ctx, fb.WorkspaceID, entitlement.ID, "access review"); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkPurchasePaid(ctx, purchase.ID, "event-1", &models.Entitlement{}); err != nil {
		t.Fatal(err)
	}
	hasAccess, err = db.HasEntitlement(ctx, fb.WorkspaceID, fb.ID, "reader-user", "", time.Now())
	if err != nil || hasAccess {
		t.Fatalf("payment replay reactivated revoked entitlement: %v, %v", hasAccess, err)
	}
	if err := db.RefundPurchase(ctx, fb.WorkspaceID, purchase.ID, "event-2", "refunded"); err != nil {
		t.Fatal(err)
	}
	sales, err := db.AggregateSales(ctx, PurchaseFilter{WorkspaceID: fb.WorkspaceID})
	if err != nil || sales.Orders != 1 || sales.GrossCents != 1000 || sales.RefundedCents != 1000 || sales.NetCents != 0 {
		t.Fatalf("sales aggregation incorrect: %+v, %v", sales, err)
	}

	collection := &models.Collection{WorkspaceID: fb.WorkspaceID, Name: "Reports", Visibility: models.VisibilityPublic}
	if err := db.CreateCollection(ctx, collection); err != nil {
		t.Fatal(err)
	}
	if err := db.AddFlipbookToCollection(ctx, fb.WorkspaceID, collection.ID, fb.ID); err != nil {
		t.Fatal(err)
	}
	current, err = db.GetFlipbook(fb.ID)
	if err != nil || len(current.CollectionIDs) != 1 || current.CollectionIDs[0] != collection.ID {
		t.Fatalf("collection membership missing: %+v, %v", current, err)
	}

	if err := db.PurgeFlipbook(ctx, fb.WorkspaceID, fb.ID); err != nil {
		t.Fatal(err)
	}
	for name, count := range map[string]int64{
		"leads":     mustCount(ctx, t, db.leads, bson.M{"flipbook_id": fb.ID}),
		"analytics": mustCount(ctx, t, db.analyticsEvents, bson.M{"flipbook_id": fb.ID}),
		"purchases": mustCount(ctx, t, db.purchases, bson.M{"flipbook_id": fb.ID}),
		"revisions": mustCount(ctx, t, db.revisions, bson.M{"flipbook_id": fb.ID}),
	} {
		if count != 0 {
			t.Fatalf("purge left %s records: %d", name, count)
		}
	}
}

func mustCount(ctx context.Context, t *testing.T, collection *mongo.Collection, filter any) int64 {
	t.Helper()
	count, err := collection.CountDocuments(ctx, filter)
	if err != nil {
		t.Fatal(fmt.Sprintf("count documents: %v", err))
	}
	return count
}

func TestIntegrationMigrationEnablesUnconfiguredEmbeds(t *testing.T) {
	uri := integrationURI(t)
	name := integrationDatabaseName()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	rawDB := client.Database(name)
	now := time.Now().UTC()
	zeroEmbed := bson.M{"enabled": false, "start_page": 1, "show_toolbar": false, "show_search": false, "show_share": false, "show_download": false}
	_, err = rawDB.Collection("workspaces").InsertOne(ctx, bson.M{
		"_id": models.DefaultWorkspaceID, "slug": models.DefaultWorkspaceID, "name": "Default Workspace",
		"owner_id": models.LegacyAdminUserID, "created_at": now, "updated_at": now,
		"settings": bson.M{"embed": zeroEmbed},
	})
	if err != nil {
		_ = client.Disconnect(ctx)
		t.Fatal(err)
	}
	book := func(id string, embed bson.M) bson.M {
		doc := bson.M{
			"_id": id, "workspace_id": models.DefaultWorkspaceID, "title": id, "slug": id,
			"filename": id + ".pdf", "status": models.StatusReady, "created_at": now, "updated_at": now,
		}
		if embed != nil {
			doc["embed"] = embed
		}
		return doc
	}
	_, err = rawDB.Collection("flipbooks").InsertMany(ctx, []any{
		book("pre-gate", nil),
		book("inherited-zero", zeroEmbed),
		book("deliberately-off", bson.M{"enabled": false, "start_page": 1, "show_toolbar": true, "show_search": true}),
		book("allowlisted-off", bson.M{"enabled": false, "start_page": 1, "allowed_domains": bson.A{"penninn.is"}}),
		book("already-on", bson.M{"enabled": true, "start_page": 3, "show_toolbar": false}),
	})
	if err != nil {
		_ = client.Disconnect(ctx)
		t.Fatal(err)
	}
	if err := client.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}

	db := openIntegrationDB(t, uri, name)
	workspace, err := db.GetWorkspace(ctx, models.DefaultWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if got := workspace.Settings.DefaultEmbed; !got.Enabled || !got.ShowToolbar || !got.ShowSearch {
		t.Fatalf("unconfigured workspace embed defaults not enabled: %+v", got)
	}
	want := map[string]models.EmbedSettings{
		"pre-gate":         {Enabled: true, StartPage: 1, ShowToolbar: true, ShowSearch: true},
		"inherited-zero":   {Enabled: true, StartPage: 1, ShowToolbar: true, ShowSearch: true},
		"deliberately-off": {Enabled: false, StartPage: 1, ShowToolbar: true, ShowSearch: true},
		"allowlisted-off":  {Enabled: false, StartPage: 1, AllowedDomains: []string{"penninn.is"}},
		"already-on":       {Enabled: true, StartPage: 3},
	}
	for id, expected := range want {
		fb, err := db.GetFlipbook(id)
		if err != nil {
			t.Fatal(err)
		}
		got := fb.Embed
		if got.Enabled != expected.Enabled || got.StartPage != expected.StartPage || got.ShowToolbar != expected.ShowToolbar ||
			got.ShowSearch != expected.ShowSearch || got.ShowShare || got.ShowDownload ||
			strings.Join(got.AllowedDomains, ",") != strings.Join(expected.AllowedDomains, ",") {
			t.Errorf("%s embed = %+v, want %+v", id, got, expected)
		}
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migration is not idempotent: %v", err)
	}
}
