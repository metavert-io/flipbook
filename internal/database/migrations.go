package database

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/metavert-io/flipbook/internal/models"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type schemaMigration struct {
	ID        string    `bson:"_id"`
	AppliedAt time.Time `bson:"applied_at"`
}

func (d *DB) Migrate(ctx context.Context) error {
	migrations := []struct {
		version string
		apply   func(context.Context) error
	}{
		{"001_foundation", d.migrateFoundation},
		{"002_embed_defaults", d.migrateEmbedDefaults},
	}
	for _, migration := range migrations {
		if err := d.applyMigration(ctx, migration.version, migration.apply); err != nil {
			return fmt.Errorf("migration %s: %w", migration.version, err)
		}
	}
	return nil
}

func (d *DB) applyMigration(ctx context.Context, version string, apply func(context.Context) error) error {
	var applied schemaMigration
	err := d.schemaMigrations.FindOne(ctx, bson.M{"_id": version}).Decode(&applied)
	if err == nil {
		return nil
	}
	if err != mongo.ErrNoDocuments {
		return err
	}
	if err := apply(ctx); err != nil {
		return err
	}
	_, err = d.schemaMigrations.InsertOne(ctx, schemaMigration{ID: version, AppliedAt: time.Now().UTC()})
	if mongo.IsDuplicateKeyError(err) {
		return nil
	}
	return err
}

// migrateEmbedDefaults turns embedding on where it was never configured.
// Workspaces were created with zero-value embed settings and every upload
// copied them, so publications answered 403 embed_disabled to anonymous
// readers on other sites while the signed-in admin preview worked. A block
// with embedding off, every display option off and no domain allowlist is
// that untouched default; anything an admin actually configured is left alone.
func (d *DB) migrateEmbedDefaults(ctx context.Context) error {
	unconfigured := func(prefix string) bson.M {
		notTrue := func(field string) bson.M { return bson.M{prefix + field: bson.M{"$ne": true}} }
		return bson.M{"$and": bson.A{
			notTrue("enabled"), notTrue("show_toolbar"), notTrue("show_search"),
			notTrue("show_share"), notTrue("show_download"),
			bson.M{prefix + "allowed_domains.0": bson.M{"$exists": false}},
		}}
	}
	defaults := models.DefaultEmbedSettings()
	enable := func(prefix string) bson.M {
		return bson.M{
			prefix + "enabled":      defaults.Enabled,
			prefix + "show_toolbar": defaults.ShowToolbar,
			prefix + "show_search":  defaults.ShowSearch,
		}
	}

	workspaceSet := enable("settings.embed.")
	workspaceSet["settings.embed.start_page"] = defaults.StartPage
	workspaces, err := d.workspaces.UpdateMany(ctx, unconfigured("settings.embed."), bson.M{"$set": workspaceSet})
	if err != nil {
		return fmt.Errorf("workspace embed defaults: %w", err)
	}
	flipbooks, err := d.flipbooks.UpdateMany(ctx, unconfigured("embed."), bson.M{"$set": enable("embed.")})
	if err != nil {
		return fmt.Errorf("flipbook embed defaults: %w", err)
	}
	log.Printf("Migration 002_embed_defaults: enabled embedding on %d workspace(s) and %d flipbook(s) that never configured it",
		workspaces.ModifiedCount, flipbooks.ModifiedCount)
	return nil
}

func (d *DB) migrateFoundation(ctx context.Context) error {
	now := time.Now().UTC()
	user := models.User{ID: models.LegacyAdminUserID, Name: "Administrator", Locale: "en", CreatedAt: now, UpdatedAt: now}
	user.Normalize()
	if _, err := d.users.UpdateOne(ctx, bson.M{"_id": user.ID}, bson.M{"$setOnInsert": user}, options.UpdateOne().SetUpsert(true)); err != nil {
		return fmt.Errorf("default user: %w", err)
	}
	workspace := models.Workspace{
		ID: models.DefaultWorkspaceID, Name: "Default Workspace", OwnerID: user.ID, CreatedAt: now, UpdatedAt: now,
		Settings: models.WorkspaceSettings{DefaultEmbed: models.DefaultEmbedSettings()},
	}
	workspace.ApplyDefaults()
	if _, err := d.workspaces.UpdateOne(ctx, bson.M{"_id": workspace.ID}, bson.M{"$setOnInsert": workspace}, options.UpdateOne().SetUpsert(true)); err != nil {
		return fmt.Errorf("default workspace: %w", err)
	}
	membership := models.Membership{ID: workspace.ID + ":" + user.ID, WorkspaceID: workspace.ID, UserID: user.ID, Role: models.RoleOwner, CreatedAt: now, UpdatedAt: now}
	if _, err := d.memberships.UpdateOne(ctx, bson.M{"_id": membership.ID}, bson.M{"$setOnInsert": membership}, options.UpdateOne().SetUpsert(true)); err != nil {
		return fmt.Errorf("default membership: %w", err)
	}

	missing := func(field string) bson.M { return bson.M{field: bson.M{"$in": bson.A{nil, ""}}} }
	updates := []struct {
		name   string
		filter bson.M
		update bson.M
	}{
		{"workspace", missing("workspace_id"), bson.M{"$set": bson.M{"workspace_id": workspace.ID}}},
		{"owner", missing("owner_id"), bson.M{"$set": bson.M{"owner_id": user.ID}}},
		{"language", missing("language"), bson.M{"$set": bson.M{"language": "en"}}},
		{"review", missing("review_state"), bson.M{"$set": bson.M{"review_state": models.ReviewNotRequired}}},
		{"currency", missing("sales.currency"), bson.M{"$set": bson.M{"sales.currency": "USD"}}},
		{"embed start", missing("embed.start_page"), bson.M{"$set": bson.M{"embed.start_page": 1}}},
	}
	for _, item := range updates {
		if _, err := d.flipbooks.UpdateMany(ctx, item.filter, item.update); err != nil {
			return fmt.Errorf("backfill %s: %w", item.name, err)
		}
	}
	if _, err := d.flipbooks.UpdateMany(ctx,
		bson.M{"$or": bson.A{bson.M{"asset_version": bson.M{"$exists": false}}, bson.M{"asset_version": bson.M{"$lt": 1}}}},
		bson.M{"$set": bson.M{"asset_version": int64(1)}}); err != nil {
		return fmt.Errorf("backfill asset version: %w", err)
	}

	publicationBackfill := []struct {
		name       string
		filter     bson.M
		state      models.PublicationState
		visibility models.Visibility
	}{
		{"published legacy", bson.M{"status": models.StatusReady, "is_public": true}, models.PublicationPublished, models.VisibilityPublic},
		{"private legacy", bson.M{"status": models.StatusReady, "is_public": bson.M{"$ne": true}}, models.PublicationUnpublished, models.VisibilityPrivate},
		{"unconverted legacy", bson.M{"status": bson.M{"$ne": models.StatusReady}}, models.PublicationDraft, models.VisibilityPublic},
	}
	for _, item := range publicationBackfill {
		filter := bson.M{"$and": bson.A{missing("publication_state"), item.filter}}
		set := bson.M{"publication_state": item.state}
		if item.state == models.PublicationPublished {
			set["published_at"] = bson.M{"$ifNull": bson.A{"$converted_at", "$updated_at"}}
		}
		if _, err := d.flipbooks.UpdateMany(ctx, filter, mongo.Pipeline{
			{{Key: "$set", Value: set}},
			{{Key: "$set", Value: bson.M{"visibility": bson.M{"$ifNull": bson.A{"$visibility", item.visibility}}}}},
		}); err != nil {
			return fmt.Errorf("%s migration: %w", item.name, err)
		}
	}
	for _, item := range []struct {
		filter     bson.M
		visibility models.Visibility
	}{{bson.M{"is_public": true}, models.VisibilityPublic}, {bson.M{"is_public": bson.M{"$ne": true}}, models.VisibilityPrivate}} {
		if _, err := d.flipbooks.UpdateMany(ctx,
			bson.M{"$and": bson.A{missing("visibility"), item.filter}},
			bson.M{"$set": bson.M{"visibility": item.visibility}}); err != nil {
			return fmt.Errorf("visibility migration: %w", err)
		}
	}

	cursor, err := d.flipbooks.Find(ctx, missing("current_revision_id"))
	if err != nil {
		return fmt.Errorf("find legacy revisions: %w", err)
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var fb models.Flipbook
		if err := cursor.Decode(&fb); err != nil {
			return err
		}
		assetVersion := fb.AssetVersion
		if assetVersion < 1 {
			assetVersion = 1
		}
		revisionStatus := models.RevisionPending
		switch fb.Status {
		case models.StatusReady:
			revisionStatus = models.RevisionActive
		case models.StatusConverting, models.StatusRegenerating:
			revisionStatus = models.RevisionConverting
		case models.StatusError:
			revisionStatus = models.RevisionFailed
		}
		revision := models.Revision{
			ID: "legacy-" + fb.ID + "-v1", WorkspaceID: fb.WorkspaceID, FlipbookID: fb.ID,
			Number: 1, AssetVersion: assetVersion, Status: revisionStatus,
			SourceFilename: fb.Filename, OriginalGridFSFileID: fb.GridFSFileID,
			SourceChecksum: fb.SourceChecksum, PageCount: fb.PageCount,
			CreatedBy: fb.OwnerID, CreatedAt: fb.CreatedAt, UpdatedAt: fb.UpdatedAt,
		}
		if revisionStatus == models.RevisionActive {
			revision.ValidatedAt = fb.ConvertedAt
			revision.ActivatedAt = fb.ConvertedAt
		}
		if _, err := d.revisions.UpdateOne(ctx, bson.M{"_id": revision.ID}, bson.M{"$setOnInsert": revision}, options.UpdateOne().SetUpsert(true)); err != nil {
			return fmt.Errorf("legacy revision %s: %w", fb.ID, err)
		}
		if _, err := d.flipbooks.UpdateOne(ctx,
			bson.M{"_id": fb.ID, "$or": bson.A{missing("current_revision_id")}},
			bson.M{"$set": bson.M{"current_revision_id": revision.ID}}); err != nil {
			return fmt.Errorf("current revision %s: %w", fb.ID, err)
		}
	}
	if err := cursor.Err(); err != nil {
		return err
	}
	if err := d.dropLegacySlugIndex(ctx); err != nil {
		return err
	}
	return nil
}

func (d *DB) dropLegacySlugIndex(ctx context.Context) error {
	cursor, err := d.flipbooks.Indexes().List(ctx)
	if err != nil {
		return err
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var spec struct {
			Name string `bson:"name"`
			Key  bson.D `bson:"key"`
		}
		if err := cursor.Decode(&spec); err != nil {
			return err
		}
		if spec.Name == "slug_1" && len(spec.Key) == 1 && spec.Key[0].Key == "slug" {
			if err := d.flipbooks.Indexes().DropOne(ctx, spec.Name); err != nil {
				return fmt.Errorf("drop legacy slug index: %w", err)
			}
		}
	}
	return cursor.Err()
}
