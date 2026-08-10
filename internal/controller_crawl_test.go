package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"testing"

	"go.uber.org/mock/gomock"
)

func TestDenormalizeWorksDoesNotRefreshDiscoveredAuthor(t *testing.T) {
	t.Parallel()

	getter := NewMockgetter(gomock.NewController(t))
	controller, err := NewController(newMemoryCache(), getter, nil, nil)
	if err != nil {
		t.Fatalf("NewController() error = %v", err)
	}

	ctx := context.Background()
	authorID := int64(123)
	workID := int64(456)
	authorBytes, err := json.Marshal(AuthorResource{ForeignID: authorID})
	if err != nil {
		t.Fatalf("json.Marshal(author) error = %v", err)
	}
	workBytes, err := json.Marshal(workResource{
		ForeignID: workID,
		Books:     []bookResource{{ForeignID: 789}},
	})
	if err != nil {
		t.Fatalf("json.Marshal(work) error = %v", err)
	}

	getter.EXPECT().GetAuthor(gomock.Any(), authorID).Return(authorBytes, nil)
	getter.EXPECT().GetWork(gomock.Any(), workID, nil).Return(workBytes, authorID, nil)

	go controller.Run(ctx)
	t.Cleanup(func() { controller.Shutdown(ctx) })

	if err := controller.denormalizeWorks(ctx, authorID, workID); err != nil {
		t.Fatalf("denormalizeWorks() error = %v", err)
	}
	waitForDenorm(controller)
}

func TestShallowAuthorFetchDoesNotOverwriteExplicitRefresh(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	authorID := int64(123)
	shallowBytes, err := json.Marshal(AuthorResource{ForeignID: authorID, Name: "shallow"})
	if err != nil {
		t.Fatalf("json.Marshal(shallow author) error = %v", err)
	}
	explicitBytes, err := json.Marshal(AuthorResource{ForeignID: authorID, Name: "explicit"})
	if err != nil {
		t.Fatalf("json.Marshal(explicit author) error = %v", err)
	}

	shallowStarted := make(chan struct{})
	releaseShallow := make(chan struct{})
	getter := NewMockgetter(gomock.NewController(t))
	first := getter.EXPECT().GetAuthor(gomock.Any(), authorID).DoAndReturn(func(context.Context, int64) ([]byte, error) {
		close(shallowStarted)
		<-releaseShallow
		return shallowBytes, nil
	})
	getter.EXPECT().GetAuthor(gomock.Any(), authorID).After(first.Call).Return(explicitBytes, nil)
	getter.EXPECT().GetAuthorBooks(gomock.Any(), authorID).Return(iter.Seq[int64](func(func(int64) bool) {}))

	controller, err := NewController(newMemoryCache(), getter, nil, nil)
	if err != nil {
		t.Fatalf("NewController() error = %v", err)
	}
	go controller.Run(ctx)
	t.Cleanup(func() { controller.Shutdown(ctx) })

	shallowResult := make(chan ttlpair, 1)
	shallowErr := make(chan error, 1)
	go func() {
		pair, err := controller.getAuthorForDenormalization(ctx, authorID)
		shallowResult <- pair
		shallowErr <- err
	}()
	<-shallowStarted

	gotExplicit, _, err := controller.GetAuthor(ctx, authorID)
	if err != nil {
		t.Fatalf("GetAuthor() error = %v", err)
	}
	if !bytes.Equal(gotExplicit, explicitBytes) {
		t.Fatalf("GetAuthor() bytes = %q, want %q", gotExplicit, explicitBytes)
	}

	close(releaseShallow)
	pair := <-shallowResult
	if err := <-shallowErr; err != nil {
		t.Fatalf("getAuthorForDenormalization() error = %v", err)
	}
	if !bytes.Equal(pair.bytes, explicitBytes) {
		t.Fatalf("shallow result = %q, want explicit result %q", pair.bytes, explicitBytes)
	}

	cachedBytes, ok := controller.cache.Get(ctx, AuthorKey(authorID))
	if !ok || !bytes.Equal(cachedBytes, explicitBytes) {
		t.Fatalf("cached author = %q, %t; want explicit result %q", cachedBytes, ok, explicitBytes)
	}
	if _, ok := controller.cache.Get(ctx, authorRefreshNeededKey(authorID)); ok {
		t.Fatal("shallow refresh marker was left behind")
	}
	waitForDenorm(controller)
}
