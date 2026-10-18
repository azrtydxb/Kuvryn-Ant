package artifact

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
)

func bundleFixture(f publishFixture) ([]Publication, [][]byte) {
	data := [][]byte{[]byte(`{"Version":"v1.0.0"}`), []byte("module example.test/team/pkg\n"), []byte("archive-content")}
	keys := []string{"module.info", "module.mod", "module.zip"}
	items := make([]Publication, 3)
	for i := range items {
		items[i] = f.publication(data[i])
		items[i].AssetKey = keys[i]
		items[i].ExpectedSource = f.source
	}
	return items, data
}
func bundleReaders(data [][]byte) []io.Reader {
	readers := make([]io.Reader, len(data))
	for i := range data {
		readers[i] = bytes.NewReader(data[i])
	}
	return readers
}

func TestPublishBundleAtomicVisibilityAndRetry(t *testing.T) {
	f := newPublishFixture(t)
	items, data := bundleFixture(f)
	calls := 0
	f.service.Storage = func(context.Context, string) (blob.Store, error) {
		return finalizeHookStore{Store: f.disk, hook: func() error { calls++; f.assertInvisible(t); return nil }}, nil
	}
	results, err := f.service.PublishBundle(context.Background(), f.principal, items, bundleReaders(data))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || len(results) != 3 {
		t.Fatal("incomplete staging/result")
	}
	for i, record := range results {
		if record.Replayed || record.Info != items[i].Info || record.VersionID != results[0].VersionID {
			t.Fatal("incoherent bundle", record)
		}
	}
	if f.count(t, "SELECT count(*) FROM versions") != 1 || f.count(t, "SELECT count(*) FROM assets") != 3 || f.count(t, "SELECT count(*) FROM audit_events WHERE action='artifact.publish'") != 3 || f.count(t, "SELECT count(*) FROM outbox_events WHERE topic='artifact.publish'") != 3 {
		t.Fatal("bundle metadata/events mismatch")
	}
	f.service.Storage = func(context.Context, string) (blob.Store, error) { return f.disk, nil }
	retry, err := f.service.PublishBundle(context.Background(), f.principal, items, bundleReaders(data))
	if err != nil {
		t.Fatal(err)
	}
	for i, record := range retry {
		if !record.Replayed || record.ID != results[i].ID {
			t.Fatal("retry changed bundle", record)
		}
	}
	if f.count(t, "SELECT count(*) FROM audit_events WHERE action='artifact.publish'") != 3 {
		t.Fatal("retry generated duplicate events")
	}
}

func TestPublishBundleConcurrentIdenticalWriters(t *testing.T) {
	f := newPublishFixture(t)
	items, data := bundleFixture(f)
	var wg sync.WaitGroup
	results := make(chan []Record, 6)
	failures := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			records, err := f.service.PublishBundle(context.Background(), f.principal, items, bundleReaders(data))
			results <- records
			failures <- err
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	fresh := 0
	var ids []Record
	for records := range results {
		if !records[0].Replayed {
			fresh++
		}
		if ids == nil {
			ids = records
		}
		for i, record := range records {
			if record.ID != ids[i].ID || record.Replayed != records[0].Replayed {
				t.Fatal("partial replay")
			}
		}
	}
	if fresh != 1 || f.count(t, "SELECT count(*) FROM assets") != 3 || f.count(t, "SELECT count(*) FROM outbox_events WHERE topic='artifact.publish'") != 3 {
		t.Fatal("concurrent duplicate bundle effects")
	}
}

func TestPublishBundleRejectsPartialExpandedAndConflictingVersions(t *testing.T) {
	for _, scenario := range []string{"partial", "expanded", "conflict"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPublishFixture(t)
			items, data := bundleFixture(f)
			ctx := context.Background()
			if scenario == "partial" {
				if _, err := f.service.Publish(ctx, f.principal, items[0], bytes.NewReader(data[0])); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := f.service.PublishBundle(ctx, f.principal, items, bundleReaders(data)); err != nil {
					t.Fatal(err)
				}
				if scenario == "expanded" {
					extra := f.publication([]byte("extra"))
					extra.AssetKey = "extra"
					if _, err := f.service.Publish(ctx, f.principal, extra, bytes.NewBufferString("extra")); err != nil {
						t.Fatal(err)
					}
				} else {
					data[1] = []byte("different")
					replacement := f.publication(data[1])
					replacement.AssetKey = items[1].AssetKey
					items[1] = replacement
				}
			}
			before := f.count(t, "SELECT count(*) FROM assets")
			if _, err := f.service.PublishBundle(ctx, f.principal, items, bundleReaders(data)); !errors.Is(err, ErrConflict) {
				t.Fatalf("expected conflict: %v", err)
			}
			if f.count(t, "SELECT count(*) FROM assets") != before {
				t.Fatal("conflict changed metadata")
			}
		})
	}
}

func TestPublishBundleFailuresLeaveNoPartialVersion(t *testing.T) {
	for _, scenario := range []string{"digest", "short", "long", "lease", "revision", "audit", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPublishFixture(t)
			items, data := bundleFixture(f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			expected := blob.ErrIntegrity
			switch scenario {
			case "digest":
				data[2] = bytes.Repeat([]byte("x"), len(data[2]))
			case "short":
				data[2] = data[2][:2]
			case "long":
				data[2] = append(data[2], 0)
			default:
				calls := 0
				f.service.Storage = func(context.Context, string) (blob.Store, error) {
					return finalizeHookStore{Store: f.disk, hook: func() error {
						calls++
						if calls != len(items) {
							return nil
						}
						var err error
						switch scenario {
						case "lease":
							_, err = f.pool.Exec(context.Background(), "UPDATE instance_writer_leases SET expires_at=clock_timestamp()-interval '1 second'")
						case "revision":
							_, err = f.pool.Exec(context.Background(), "UPDATE feeds SET revision=revision+1 WHERE id=$1", f.feed)
						case "audit":
							_, err = f.pool.Exec(context.Background(), `ALTER TABLE outbox_events ADD CONSTRAINT reject_second_asset CHECK(topic<>'artifact.publish' OR payload->>'digest'<>'`+string(items[1].Info.Digest)+`')`)
						case "cancel":
							cancel()
						}
						return err
					}}, nil
				}
				switch scenario {
				case "lease":
					expected = ownership.ErrLost
				case "revision":
					expected = ErrConflict
				case "audit":
					expected = ErrUnavailable
				case "cancel":
					expected = ErrUnavailable
				}
			}
			if _, err := f.service.PublishBundle(ctx, f.principal, items, bundleReaders(data)); !errors.Is(err, expected) {
				t.Fatalf("expected %v, got %v", expected, err)
			}
			f.assertInvisible(t)
		})
	}
}

func TestPublishBundleRejectsInvalidAndUnauthorizedBeforeReading(t *testing.T) {
	f := newPublishFixture(t)
	original, _ := bundleFixture(f)
	for _, scenario := range []string{"mixed-version", "duplicate-key", "different-source", "scope"} {
		t.Run(scenario, func(t *testing.T) {
			items := append([]Publication(nil), original...)
			p := f.principal
			expected := ErrInput
			switch scenario {
			case "mixed-version":
				items[2].Version = "2.0"
			case "duplicate-key":
				items[2].AssetKey = items[0].AssetKey
			case "different-source":
				items[2].ExpectedSource = "another-source"
				expected = ErrConflict
			case "scope":
				p.Scopes = []identity.Scope{{Actions: []identity.Action{identity.Read}}}
				expected = ErrDenied
			}
			counted := []*countedReader{{}, {}, {}}
			readers := []io.Reader{counted[0], counted[1], counted[2]}
			if _, err := f.service.PublishBundle(context.Background(), p, items, readers); !errors.Is(err, expected) {
				t.Fatalf("expected %v, got %v", expected, err)
			}
			for _, reader := range counted {
				if reader.reads.Load() != 0 {
					t.Fatal("rejected bundle consumed input")
				}
			}
		})
	}
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: f.source, Kind: routing.Local}, {ID: "other", Kind: routing.Local}}, DefaultTarget: f.source, Rules: []routing.Rule{{ID: "split", Target: "other", Selector: routing.Selector{PathPrefix: "/module.zip"}}}})
	if err != nil {
		t.Fatal(err)
	}
	f.service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	items := append([]Publication(nil), original...)
	for i := range items {
		items[i].ExpectedSource = ""
	}
	counted := &countedReader{}
	if _, err := f.service.PublishBundle(context.Background(), f.principal, items, []io.Reader{counted, counted, counted}); !errors.Is(err, ErrConflict) || counted.reads.Load() != 0 {
		t.Fatal("split destinations accepted", err)
	}
}

func TestPublishBundleDependenciesAndTagReplay(t *testing.T) {
	f := newPublishFixture(t)
	items, data := bundleFixture(f)
	items[0].References = []Reference{{Version: items[2].Version, AssetKey: items[2].AssetKey, Info: items[2].Info}}
	items[0].Tag = "latest"
	if _, err := f.service.PublishBundle(context.Background(), f.principal, items, bundleReaders(data)); err != nil {
		t.Fatal("forward bundle reference failed", err)
	}
	before := f.count(t, "SELECT count(*) FROM audit_events WHERE action LIKE 'artifact.%'")
	if _, err := f.service.PublishBundle(context.Background(), f.principal, items, bundleReaders(data)); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "SELECT count(*) FROM audit_events WHERE action LIKE 'artifact.%'") != before {
		t.Fatal("tag retry emitted events")
	}
	items[0].Tag = "different"
	if _, err := f.service.PublishBundle(context.Background(), f.principal, items, bundleReaders(data)); !errors.Is(err, ErrConflict) {
		t.Fatal("bundle retry added a tag", err)
	}
}
