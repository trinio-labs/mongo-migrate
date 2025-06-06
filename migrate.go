// Package migrate allows to perform versioned migrations in your MongoDB.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type collectionSpecification struct {
	Name string `bson:"name"`
	Type string `bson:"type"`
}

type versionRecord struct {
	Version     uint64    `bson:"version"`
	Description string    `bson:"description,omitempty"`
	Timestamp   time.Time `bson:"timestamp"`
}

const defaultMigrationsCollection = "migrations"

// AllAvailable used in "Up" or "Down" methods to run all available migrations.
const AllAvailable = -1

// Migrate is type for performing migrations in provided database.
// Database versioned using dedicated collection.
// Each migration applying ("up" and "down") adds new document to collection.
// This document consists migration version, migration description and timestamp.
// Current database version determined as version in latest added document (biggest "_id") from collection mentioned above.
type Migrate struct {
	db                   *mongo.Database
	migrations           []Migration
	migrationsCollection string
	log                  Logger
}

func NewMigrate(db *mongo.Database, migrations ...Migration) *Migrate {
	internalMigrations := make([]Migration, len(migrations))
	copy(internalMigrations, migrations)
	return &Migrate{
		db:                   db,
		migrations:           internalMigrations,
		migrationsCollection: defaultMigrationsCollection,
	}
}

// SetMigrationsCollection replaces name of collection for storing migration information.
// By default, it is "migrations".
func (m *Migrate) SetMigrationsCollection(name string) {
	m.migrationsCollection = name
}

func (m *Migrate) isCollectionExist(ctx context.Context, name string) (isExist bool, err error) {
	collections, err := m.getCollections(ctx)
	if err != nil {
		return false, err
	}

	for _, c := range collections {
		if name == c.Name {
			return true, nil
		}
	}
	return false, nil
}

func (m *Migrate) createCollectionIfNotExist(ctx context.Context, name string) error {
	exist, err := m.isCollectionExist(ctx, name)
	if err != nil {
		return err
	}
	if exist {
		return nil
	}

	command := bson.D{bson.E{Key: "create", Value: name}}
	if err = m.db.RunCommand(ctx, command).Err(); err != nil {
		return err
	}

	return nil
}

func (m *Migrate) getCollections(ctx context.Context) (collections []collectionSpecification, err error) {
	cursor, err := m.db.ListCollections(ctx, bson.D{})
	if err != nil {
		return nil, err
	}

	if cursor != nil {
		defer func(cursor *mongo.Cursor) {
			curErr := cursor.Close(ctx)
			if curErr != nil {
				if err != nil {
					err = fmt.Errorf("migrate: get collection failed: %w", err)
				} else {
					err = curErr
				}
			}
		}(cursor)
	}

	for cursor.Next(ctx) {
		var collection collectionSpecification

		err := cursor.Decode(&collection)
		if err != nil {
			return nil, err
		}

		if len(collection.Type) == 0 || collection.Type == "collection" {
			collections = append(collections, collection)
		}
	}

	if err := cursor.Err(); err != nil {
		return nil, err
	}

	return
}

// Version returns current database version and comment.
func (m *Migrate) Version(ctx context.Context) (uint64, string, error) {
	if err := m.createCollectionIfNotExist(ctx, m.migrationsCollection); err != nil {
		return 0, "", err
	}

	filter := bson.D{{}}
	sort := bson.D{bson.E{Key: "_id", Value: -1}}
	opts := options.FindOne().SetSort(sort)

	// find record with the greatest id (assuming it`s latest also)
	result := m.db.Collection(m.migrationsCollection).FindOne(ctx, filter, opts)
	err := result.Err()
	switch {
	case errors.Is(err, mongo.ErrNoDocuments):
		return 0, "", nil
	case err != nil:
		return 0, "", err
	}

	var rec versionRecord
	if err := result.Decode(&rec); err != nil {
		return 0, "", err
	}

	return rec.Version, rec.Description, nil
}

// SetVersion forcibly changes database version to provided one.
func (m *Migrate) SetVersion(ctx context.Context, version uint64, description string) error {
	rec := versionRecord{
		Version:     version,
		Timestamp:   time.Now().UTC(),
		Description: description,
	}

	_, err := m.db.Collection(m.migrationsCollection).InsertOne(ctx, rec)
	if err != nil {
		return err
	}

	return nil
}

// Up performs "up" migrations to latest available version.
// If n<=0 all "up" migrations with newer versions will be performed.
// If n>0 only n migrations with newer version will be performed.
func (m *Migrate) Up(ctx context.Context, n int) error {
	currentVersion, _, err := m.Version(ctx)
	if err != nil {
		return err
	}
	if n <= 0 || n > len(m.migrations) {
		n = len(m.migrations)
	}
	migrationSort(m.migrations)

	for i, p := 0, 0; i < len(m.migrations) && p < n; i++ {
		migration := m.migrations[i]
		if migration.Version <= currentVersion || migration.Up == nil {
			continue
		}
		p++
		if err := migration.Up(ctx, m.db); err != nil {
			return err
		}
		if err := m.SetVersion(ctx, migration.Version, migration.Description); err != nil {
			return err
		}

		m.printUp(migration.Version, migration.Description)
	}
	return nil
}

// Down performs "down" migration to the oldest available version.
// If n<=0 all "down" migrations with older version will be performed.
// If n>0 only n migrations with older version will be performed.
func (m *Migrate) Down(ctx context.Context, n int) error {
	currentVersion, _, err := m.Version(ctx)
	if err != nil {
		return err
	}
	if n <= 0 || n > len(m.migrations) {
		n = len(m.migrations)
	}
	migrationSort(m.migrations)

	for i, p := len(m.migrations)-1, 0; i >= 0 && p < n; i-- {
		migration := m.migrations[i]
		if migration.Version > currentVersion || migration.Down == nil {
			continue
		}
		p++
		if err := migration.Down(ctx, m.db); err != nil {
			return err
		}

		var prevMigration Migration
		if i == 0 {
			prevMigration = Migration{Version: 0}
		} else {
			prevMigration = m.migrations[i-1]
		}
		if err := m.SetVersion(ctx, prevMigration.Version, prevMigration.Description); err != nil {
			return err
		}

		m.printDown(migration.Version, migration.Description)
	}
	return nil
}

// SetLogger sets a logger to print the migration process
func (m *Migrate) SetLogger(log Logger) {
	m.log = log
}

func (m *Migrate) printUp(migrationVersion uint64, migrationDescription string) {
	m.printf("Migrated UP: %d %s", migrationVersion, migrationDescription)
}

func (m *Migrate) printDown(migrationVersion uint64, migrationDescription string) {
	m.printf("Migrated DOWN: %d %s", migrationVersion, migrationDescription)
}

func (m *Migrate) printf(msg string, args ...any) {
	if m.log == nil {
		return
	}

	m.log.Printf(msg, args...)
}
