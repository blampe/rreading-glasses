package internal

import (
	"context"
	"encoding/json"
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
